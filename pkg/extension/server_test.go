package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testNoop(context.Context, Call, *Stream) (any, error) { return struct{}{}, nil }

func operationModule(handler Handler) Module {
	return Module{
		Manifest: testManifest(OperationControl),
		Handlers: map[string]Handler{MethodOperationStatus: handler, MethodOperationCancel: testNoop},
	}
}

type testPeer struct {
	t          *testing.T
	input      *io.PipeWriter
	output     *io.PipeReader
	encoder    *json.Encoder
	sendMu     sync.Mutex
	frames     chan Frame
	done       chan struct{}
	readerDone chan struct{}
	err        error
	cancel     context.CancelFunc
}

func startPeer(t *testing.T, module Module) *testPeer {
	t.Helper()
	input, send := io.Pipe()
	receive, output := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	peer := &testPeer{
		t: t, input: send, output: receive, encoder: json.NewEncoder(send), frames: make(chan Frame, MaxConcurrent*4),
		done: make(chan struct{}), readerDone: make(chan struct{}), cancel: cancel,
	}
	go func() {
		defer close(peer.done)
		defer input.Close()
		defer output.Close()
		peer.err = Serve(ctx, input, output, module)
	}()
	go func() {
		defer close(peer.readerDone)
		defer close(peer.frames)
		decoder := json.NewDecoder(receive)
		for {
			var frame Frame
			if decoder.Decode(&frame) != nil {
				return
			}
			select {
			case peer.frames <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		send.Close()
		receive.Close()
		for _, done := range []<-chan struct{}{peer.done, peer.readerDone} {
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("extension test left a transport or worker goroutine running")
			}
		}
	})
	return peer
}

func (peer *testPeer) send(frame Frame) {
	peer.t.Helper()
	peer.sendMu.Lock()
	defer peer.sendMu.Unlock()
	if err := peer.encoder.Encode(frame); err != nil {
		peer.t.Fatal("sending frame:", err)
	}
}

func (peer *testPeer) read() Frame {
	peer.t.Helper()
	select {
	case frame, found := <-peer.frames:
		if !found {
			peer.t.Fatal("extension closed its output before the expected frame")
		}
		return frame
	case <-time.After(15 * time.Second):
		peer.t.Fatal("extension did not produce the expected frame")
		return Frame{}
	}
}

func (peer *testPeer) wait() error {
	peer.t.Helper()
	select {
	case <-peer.done:
		return peer.err
	case <-time.After(15 * time.Second):
		peer.t.Fatal("extension did not stop")
		return nil
	}
}

func callFrame(id, method string, params any) Frame {
	body, _ := json.Marshal(params)
	return Frame{Kind: KindCall, ID: id, Method: method, Params: body, Scope: Scope{Plugin: "fixture"}}
}

func TestServeDescribesAndRefusesUnknownMethodsWithoutExposingPrivateErrors(t *testing.T) {
	module := operationModule(func(_ context.Context, call Call, _ *Stream) (any, error) {
		if string(call.Params) == `"panic"` {
			panic("private panic credential")
		}
		return nil, errors.New("private error credential")
	})
	module.Manifest.Args = []string{"--private-packaging"}
	peer := startPeer(t, module)
	peer.send(callFrame("describe", MethodDescribe, nil))
	described := peer.read()
	var actual Manifest
	if described.Error != nil || json.Unmarshal(described.Result, &actual) != nil ||
		actual.Runtime != "" || actual.Entry != "" || actual.Args != nil || !reflect.DeepEqual(actual, module.Manifest.Public()) {
		t.Fatalf("describe did not return public manifest authority: %+v", described)
	}
	for _, test := range []struct{ id, method, params, code string }{
		{"unknown", "private.admin", `null`, "method_not_found"},
		{"private", MethodOperationStatus, `null`, "internal"},
		{"panic", MethodOperationStatus, `"panic"`, "internal"},
	} {
		peer.send(Frame{Kind: KindCall, ID: test.id, Method: test.method, Params: json.RawMessage(test.params)})
		frame := peer.read()
		body, _ := json.Marshal(frame)
		if frame.Kind != KindResult || frame.ID != test.id || frame.Error == nil || frame.Error.Code != test.code || strings.Contains(string(body), "credential") {
			t.Fatalf("unsafe or wrong refusal: %s", body)
		}
	}
}

func TestStreamAcknowledgementsApplyPerCallWithoutBlockingMultiplexing(t *testing.T) {
	returned := make(chan struct{})
	peer := startPeer(t, operationModule(func(ctx context.Context, call Call, stream *Stream) (any, error) {
		if call.ID != "stream" {
			return "independent", nil
		}
		if err := stream.Emit(ctx, "data", []byte("first")); err != nil {
			return nil, err
		}
		close(returned)
		if err := stream.Emit(ctx, "data", []byte("second")); err != nil {
			return nil, err
		}
		return "complete", nil
	}))
	peer.send(callFrame("stream", MethodOperationStatus, nil))
	first := peer.read()
	if first.Kind != KindEvent || first.ID != "stream" || first.Seq != 1 {
		t.Fatalf("first event: %+v", first)
	}
	select {
	case <-returned:
		t.Fatal("Emit returned before acknowledgement")
	default:
	}
	peer.send(callFrame("other", MethodOperationStatus, nil))
	independent := peer.read()
	if independent.Kind != KindResult || independent.ID != "other" {
		t.Fatalf("another call blocked, or a second event escaped credit: %+v", independent)
	}
	peer.send(Frame{Kind: KindAck, ID: first.ID, Seq: first.Seq})
	second := peer.read()
	if second.Kind != KindEvent || second.ID != "stream" || second.Seq != 2 {
		t.Fatalf("second event: %+v", second)
	}
	select {
	case <-returned:
	default:
		t.Fatal("acknowledged Emit did not return before the next event")
	}
	peer.send(Frame{Kind: KindAck, ID: second.ID, Seq: second.Seq})
	terminal := peer.read()
	if terminal.Kind != KindResult || terminal.ID != "stream" || terminal.Error != nil {
		t.Fatalf("terminal result: %+v", terminal)
	}
}

func TestHostCallUsesFrozenParentScopeAndSeparatesSelectedAuth(t *testing.T) {
	received := make(chan Call, 1)
	peer := startPeer(t, operationModule(func(ctx context.Context, call Call, stream *Stream) (any, error) {
		received <- call
		call.Scope.Account = "forged"
		var state StateValue
		err := stream.HostCall(ctx, MethodStateGet, StateGetRequest{Key: "state"}, &state)
		return state, err
	}))
	frame := callFrame("parent", MethodOperationStatus, nil)
	frame.Scope = Scope{Plugin: "fixture", Provider: "provider", Account: "account", Generation: 17, Session: "session", Operation: "op", Attempt: 4}
	frame.Auth = json.RawMessage(`{"token":"selected-only"}`)
	frame.Deadline = time.Now().Add(time.Minute).UnixMilli()
	peer.send(frame)
	callback := peer.read()
	if callback.Kind != KindHostCall || callback.ID == frame.ID || callback.Parent != frame.ID || callback.Method != MethodStateGet ||
		callback.Scope != frame.Scope || callback.Deadline != frame.Deadline || callback.Auth != nil {
		t.Fatalf("callback escaped parent authority or leaked auth: %+v", callback)
	}
	select {
	case call := <-received:
		if !bytes.Equal(call.Auth, frame.Auth) {
			t.Fatal("handler did not receive selected account auth")
		}
	default:
		t.Fatal("handler was not invoked")
	}
	peer.send(Frame{Kind: KindHostResult, ID: callback.ID, Parent: callback.Parent,
		Result: json.RawMessage(`{"found":true,"version":"7","value":{"count":2}}`)})
	result := peer.read()
	var state StateValue
	if result.Error != nil || result.ID != frame.ID || json.Unmarshal(result.Result, &state) != nil || state.Version != "7" || !state.Found {
		t.Fatalf("host result was not delivered: %+v", result)
	}
}

func TestHostResultMustMatchTheExactChildAndParent(t *testing.T) {
	for _, parent := range []string{"another-parent", ""} {
		t.Run(parent, func(t *testing.T) {
			peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
				return nil, stream.HostCall(ctx, MethodStateGet, StateGetRequest{Key: "state"}, nil)
			}))
			peer.send(callFrame("parent", MethodOperationStatus, nil))
			callback := peer.read()
			peer.send(Frame{Kind: KindHostResult, ID: callback.ID, Parent: parent, Result: json.RawMessage(`null`)})
			var public *Error
			if err := peer.wait(); !errors.As(err, &public) || public.Code != "protocol_error" {
				t.Fatal("host result crossed its parent", err)
			}
		})
	}
	peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
		var state StateValue
		err := stream.HostCall(ctx, MethodStateGet, StateGetRequest{Key: "state"}, &state)
		return state, err
	}))
	peer.send(callFrame("parent", MethodOperationStatus, nil))
	callback := peer.read()
	peer.send(Frame{Kind: KindHostResult, ID: callback.ID + "suffix", Parent: callback.Parent, Result: json.RawMessage(`{"version":"forged"}`)})
	var public *Error
	if err := peer.wait(); !errors.As(err, &public) || public.Code != "protocol_error" {
		t.Fatal("a prefixed callback id was accepted", err)
	}
}

func TestHostCallsAreSerializedWithinTheirParent(t *testing.T) {
	peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
		results := make(chan error, 2)
		for range 2 {
			go func() {
				results <- stream.HostCall(ctx, MethodStateGet, StateGetRequest{Key: "state"}, nil)
			}()
		}
		var result error
		for range 2 {
			if err := <-results; err != nil {
				result = err
			}
		}
		return nil, result
	}))
	peer.send(callFrame("parent", MethodOperationStatus, nil))
	first := peer.read()
	if first.Kind != KindHostCall {
		t.Fatal("missing first callback", first)
	}
	peer.send(callFrame("barrier", MethodOperationCancel, nil))
	if barrier := peer.read(); barrier.Kind != KindResult || barrier.ID != "barrier" {
		t.Fatal("a second unconsumed callback escaped its parent's bound", barrier)
	}
	peer.send(Frame{Kind: KindHostResult, ID: first.ID, Parent: first.Parent, Result: json.RawMessage(`null`)})
	second := peer.read()
	if second.Kind != KindHostCall || second.ID == first.ID || second.Parent != first.Parent {
		t.Fatal("second callback did not retain parent isolation", second)
	}
	peer.send(Frame{Kind: KindHostResult, ID: second.ID, Parent: second.Parent, Result: json.RawMessage(`null`)})
	if terminal := peer.read(); terminal.Kind != KindResult || terminal.ID != first.Parent || terminal.Error != nil {
		t.Fatal("parent did not terminate after its callbacks", terminal)
	}
}

func TestHostCallbackSupportsMaximumLengthParentID(t *testing.T) {
	peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
		return nil, stream.HostCall(ctx, MethodStateGet, StateGetRequest{Key: "state"}, nil)
	}))
	parent := strings.Repeat("x", 256)
	peer.send(callFrame(parent, MethodOperationStatus, nil))
	callback := peer.read()
	if len(callback.ID) > 256 || callback.Parent != parent {
		t.Fatalf("SDK produced a callback id its own parser refuses: child bytes=%d parent bytes=%d", len(callback.ID), len(callback.Parent))
	}
	peer.send(Frame{Kind: KindHostResult, ID: callback.ID, Parent: parent, Result: json.RawMessage(`null`)})
	if result := peer.read(); result.Kind != KindResult || result.ID != parent || result.Error != nil {
		t.Fatal("maximum-length parent did not complete", result)
	}
}

func TestCancelUnblocksEventAndHostCallEvenWithBackgroundContext(t *testing.T) {
	for _, hostCall := range []bool{false, true} {
		t.Run(fmt.Sprint(hostCall), func(t *testing.T) {
			peer := startPeer(t, operationModule(func(_ context.Context, call Call, stream *Stream) (any, error) {
				if call.ID == "other" {
					return "alive", nil
				}
				if hostCall {
					return nil, stream.HostCall(context.Background(), MethodStateGet, StateGetRequest{Key: "state"}, nil)
				}
				return nil, stream.Emit(context.Background(), "data", []byte("waiting"))
			}))
			peer.send(callFrame("cancel", MethodOperationStatus, nil))
			waiting := peer.read()
			if waiting.Kind != KindEvent && waiting.Kind != KindHostCall {
				t.Fatalf("call was not waiting: %+v", waiting)
			}
			peer.send(Frame{Kind: KindCancel, ID: "cancel"})
			result := peer.read()
			if result.ID != "cancel" || result.Kind != KindResult || result.Error == nil || result.Error.Code != "canceled" {
				t.Fatalf("canceled call: %+v", result)
			}
			peer.send(callFrame("other", MethodOperationStatus, nil))
			if frame := peer.read(); frame.Error != nil || frame.ID != "other" {
				t.Fatalf("cancellation stopped another call: %+v", frame)
			}
		})
	}
}

func TestCallDeadlineIsInContextAndExpiredCallsDoNotInvokeHandler(t *testing.T) {
	var calls atomic.Int64
	peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, _ *Stream) (any, error) {
		calls.Add(1)
		deadline, found := ctx.Deadline()
		if !found {
			return nil, &Error{Code: "invalid_params", Message: "deadline was lost"}
		}
		return deadline.UnixMilli(), nil
	}))
	expired := callFrame("expired", MethodOperationStatus, nil)
	expired.Deadline = time.Now().Add(-time.Hour).UnixMilli()
	peer.send(expired)
	result := peer.read()
	if result.Error == nil || result.Error.Code != "deadline" || calls.Load() != 0 {
		t.Fatalf("expired call ran: %+v calls=%d", result, calls.Load())
	}
	future := callFrame("future", MethodOperationStatus, nil)
	future.Deadline = time.Now().Add(time.Hour).UnixMilli()
	peer.send(future)
	result = peer.read()
	var deadline int64
	if result.Error != nil || json.Unmarshal(result.Result, &deadline) != nil || deadline != future.Deadline || calls.Load() != 1 {
		t.Fatalf("deadline was not preserved: %+v calls=%d", result, calls.Load())
	}
}

func TestServerCloseCancelsWaitingCallsAndJoinsWorkers(t *testing.T) {
	for _, byEOF := range []bool{false, true} {
		t.Run(fmt.Sprint(byEOF), func(t *testing.T) {
			exited := make(chan struct{})
			peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
				defer close(exited)
				return nil, stream.HostCall(ctx, MethodStateGet, StateGetRequest{Key: "state"}, nil)
			}))
			peer.send(callFrame("waiting", MethodOperationStatus, nil))
			if frame := peer.read(); frame.Kind != KindHostCall {
				t.Fatal("handler did not reach host service", frame)
			}
			if byEOF {
				peer.input.Close()
			} else {
				peer.cancel()
			}
			err := peer.wait()
			if byEOF && err != nil || !byEOF && !errors.Is(err, context.Canceled) {
				t.Fatal("unexpected server close result", err)
			}
			select {
			case <-exited:
			default:
				t.Fatal("Serve returned before the waiting handler ended")
			}
		})
	}
}

func TestServerRejectsDuplicateLiveIDsAndUnknownAcknowledgements(t *testing.T) {
	for _, test := range []string{"duplicate", "unknown ack", "wrong sequence"} {
		t.Run(test, func(t *testing.T) {
			peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
				return nil, stream.Emit(ctx, "data", []byte("waiting"))
			}))
			peer.send(callFrame("live", MethodOperationStatus, nil))
			event := peer.read()
			switch test {
			case "duplicate":
				peer.send(callFrame("live", MethodOperationStatus, nil))
			case "unknown ack":
				peer.send(Frame{Kind: KindAck, ID: "absent", Seq: 1})
			case "wrong sequence":
				peer.send(Frame{Kind: KindAck, ID: event.ID, Seq: event.Seq + 1})
			}
			var public *Error
			if err := peer.wait(); !errors.As(err, &public) || public.Code != "protocol_error" {
				t.Fatal("invalid transport state was accepted", err)
			}
		})
	}
}

func TestServerBoundsConcurrentCalls(t *testing.T) {
	started := make(chan struct{}, MaxConcurrent)
	var active, peak atomic.Int64
	peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, _ *Stream) (any, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); current > previous; previous = peak.Load() {
			if peak.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	for index := range MaxConcurrent {
		peer.send(callFrame(fmt.Sprint(index), MethodOperationStatus, nil))
	}
	for range MaxConcurrent {
		select {
		case <-started:
		case <-time.After(15 * time.Second):
			t.Fatal("bounded worker did not start")
		}
	}
	peer.send(callFrame("overflow", MethodOperationStatus, nil))
	var public *Error
	if err := peer.wait(); !errors.As(err, &public) || public.Code != "resource_exhausted" || peak.Load() != MaxConcurrent || active.Load() != 0 {
		t.Fatalf("worker bound: error=%v peak=%d active=%d", err, peak.Load(), active.Load())
	}
}

func TestConcurrentStreamsSerializeStdoutAndRetainCallIDs(t *testing.T) {
	peer := startPeer(t, operationModule(func(ctx context.Context, call Call, stream *Stream) (any, error) {
		for _, event := range []string{"headers", "accepted", "data", "flush"} {
			var data any = map[string]string{"operationId": call.ID}
			if event == "data" {
				data = []byte(call.ID)
			}
			if err := stream.Emit(ctx, event, data); err != nil {
				return nil, err
			}
		}
		return call.ID, nil
	}))
	const count = 16
	for index := range count {
		peer.send(callFrame(fmt.Sprint(index), MethodOperationStatus, nil))
	}
	sequence := map[string]uint64{}
	results := map[string]bool{}
	for len(results) < count {
		frame := peer.read()
		switch frame.Kind {
		case KindEvent:
			if frame.Seq != sequence[frame.ID]+1 {
				t.Fatal("interleaved or duplicated event", frame)
			}
			sequence[frame.ID] = frame.Seq
			if frame.Event == "accepted" && (len(frame.Data) == 0 || frame.Data[0] != '{') {
				t.Fatal("acceptance data was not JSON", frame)
			}
			peer.send(Frame{Kind: KindAck, ID: frame.ID, Seq: frame.Seq})
		case KindResult:
			var id string
			if frame.Error != nil || json.Unmarshal(frame.Result, &id) != nil || id != frame.ID || sequence[id] != 4 || results[id] {
				t.Fatal("wrong or duplicate terminal result", frame)
			}
			results[id] = true
		default:
			t.Fatal("invalid stdout frame", frame)
		}
	}
}

func TestFrameAndChunkBounds(t *testing.T) {
	for _, size := range []int{MaxChunkBytes, MaxChunkBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			peer := startPeer(t, operationModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
				return nil, stream.Emit(ctx, "data", bytes.Repeat([]byte{42}, size))
			}))
			peer.send(callFrame("chunk", MethodOperationStatus, nil))
			frame := peer.read()
			if size <= MaxChunkBytes {
				var decoded []byte
				if frame.Kind != KindEvent || json.Unmarshal(frame.Data, &decoded) != nil || len(decoded) != size {
					t.Fatal("exact-sized data chunk was refused", frame)
				}
				peer.send(Frame{Kind: KindAck, ID: frame.ID, Seq: frame.Seq})
				frame = peer.read()
				if frame.Error != nil {
					t.Fatal(frame.Error)
				}
			} else if frame.Kind != KindResult || frame.Error == nil || frame.Error.Code != "resource_exhausted" {
				t.Fatal("oversized data chunk escaped", frame)
			}
		})
	}
	t.Run("exact frame", func(t *testing.T) {
		peer := startPeer(t, operationModule(func(_ context.Context, call Call, _ *Stream) (any, error) {
			return len(call.Params), nil
		}))
		frame := callFrame("large", MethodOperationStatus, "")
		initial, _ := json.Marshal(frame)
		frame.Params = json.RawMessage(`"` + strings.Repeat("x", MaxFrameBytes-len(initial)) + `"`)
		encoded, _ := json.Marshal(frame)
		if len(encoded) != MaxFrameBytes {
			t.Fatal("bad boundary fixture", len(encoded))
		}
		peer.send(frame)
		if frame := peer.read(); frame.Error != nil || frame.Kind != KindResult {
			t.Fatal("exact-sized frame refused", frame)
		}
	})
	t.Run("oversized frame", func(t *testing.T) {
		peer := startPeer(t, operationModule(testNoop))
		io.WriteString(peer.input, strings.Repeat(" ", MaxFrameBytes+1)+"\n")
		var public *Error
		if err := peer.wait(); !errors.As(err, &public) || public.Code != "protocol_error" {
			t.Fatal("oversized envelope accepted", err)
		}
	})
	t.Run("oversized result", func(t *testing.T) {
		peer := startPeer(t, operationModule(func(context.Context, Call, *Stream) (any, error) {
			return strings.Repeat("x", MaxFrameBytes), nil
		}))
		peer.send(callFrame("result", MethodOperationStatus, nil))
		if frame := peer.read(); frame.Error == nil || frame.Error.Code != "resource_exhausted" {
			t.Fatal("oversized result escaped", frame)
		}
	})
}

func TestInvalidJSONIsTerminalAndDoesNotRunHandlers(t *testing.T) {
	for _, invalid := range []string{`{`, `{"kind":"call","id":"bad"} {}`, `{"kind":"call","id":"bad","method":"operation.status","unexpected":true}`} {
		var calls atomic.Int64
		peer := startPeer(t, operationModule(func(context.Context, Call, *Stream) (any, error) {
			calls.Add(1)
			return nil, nil
		}))
		io.WriteString(peer.input, invalid+"\n")
		var public *Error
		if err := peer.wait(); !errors.As(err, &public) || public.Code != "protocol_error" || calls.Load() != 0 {
			t.Fatalf("invalid JSON reached a handler: %v calls=%d", err, calls.Load())
		}
	}
}
