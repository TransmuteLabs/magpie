package native

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
	codec "github.com/yetone/magpie/pkg/protocolext"
)

func fixtureModule(handler sdk.Handler) sdk.Module {
	return sdk.Module{
		Manifest: sdk.Manifest{
			Schema: sdk.ManifestSchema, ID: "native-fixture", Version: "1.0.0",
			Runtime: "native", Entry: "plugin.so",
			Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1}, Capabilities: []sdk.Capability{sdk.ToolProvider}},
			Tools:    []sdk.ToolDescriptor{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		},
		Handlers: map[string]sdk.Handler{sdk.MethodToolCall: handler},
	}
}

func echo(context.Context, sdk.Call, *sdk.Stream) (any, error) {
	return map[string]bool{"ok": true}, nil
}

type peer struct {
	lib    *nativeLibrary
	id     uint64
	state  *session
	frames chan sdk.Frame
	t      *testing.T
}

func newPeer(t *testing.T, module sdk.Module) *peer {
	t.Helper()
	lib := newLibrary()
	if err := lib.register(module); err != nil {
		t.Fatal(err)
	}
	return openPeer(t, lib)
}

func openPeer(t *testing.T, lib *nativeLibrary) *peer {
	t.Helper()
	peer := &peer{t: t, lib: lib, frames: make(chan sdk.Frame, 128)}
	var err error
	peer.id, err = lib.open(func(body []byte) error {
		var frame sdk.Frame
		if err := json.Unmarshal(bytes.TrimSpace(body), &frame); err != nil {
			return err
		}
		peer.frames <- frame
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	peer.state = lib.get(peer.id)
	t.Cleanup(func() {
		_ = lib.close(peer.id)
		peer.state.stop()
		wait(t, peer.state.done, "native server shutdown")
	})
	return peer
}

func (peer *peer) send(frame sdk.Frame) {
	peer.t.Helper()
	body, err := json.Marshal(frame)
	if err != nil {
		peer.t.Fatal(err)
	}
	if err := peer.lib.send(peer.id, append(body, '\n')); err != nil {
		peer.t.Fatal(err)
	}
}

func (peer *peer) read() sdk.Frame {
	peer.t.Helper()
	select {
	case frame := <-peer.frames:
		return frame
	case <-time.After(5 * time.Second):
		peer.t.Fatal("native callback did not deliver a frame")
		return sdk.Frame{}
	}
}

func wait(t *testing.T, done <-chan struct{}, reason string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal(reason + " did not finish")
	}
}

func call(id, method string) sdk.Frame {
	return sdk.Frame{Kind: sdk.KindCall, ID: id, Method: method, Params: json.RawMessage(`{}`),
		Scope: sdk.Scope{Plugin: "native-fixture", Session: id}}
}

func TestRegistrationRejectsInvalidAndRepeatedModules(t *testing.T) {
	for _, mutate := range []struct {
		name string
		edit func(*sdk.Module)
	}{
		{"exec", func(module *sdk.Module) { module.Manifest.Runtime = "exec" }},
		{"version", func(module *sdk.Module) { module.Manifest.Version = "invalid" }},
		{"oversized-id", func(module *sdk.Module) { module.Manifest.ID = strings.Repeat("x", 5000) }},
		{"handler", func(module *sdk.Module) { delete(module.Handlers, sdk.MethodToolCall) }},
		{"undeclared", func(module *sdk.Module) { module.Handlers["private.method"] = echo }},
		{"panic", func(module *sdk.Module) {
			module.Manifest.Contract.Capabilities = append(module.Manifest.Contract.Capabilities, sdk.InboundCodec)
			module.Manifest.Inbounds = []sdk.InboundDescriptor{{ID: "codec", Path: "/native-fixture/rpc"}}
			module.Inbounds = []codec.Inbound{panicInbound{}}
		}},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			lib, module := newLibrary(), fixtureModule(echo)
			mutate.edit(&module)
			if err := lib.register(module); err == nil || lib.module != nil {
				t.Fatal("invalid module was published")
			} else if len(err.Error()) > 4096 {
				t.Fatal("registration returned an unbounded validation error")
			}
			if err := lib.register(fixtureModule(echo)); err != nil {
				t.Fatal("failure prevented a valid registration:", err)
			}
		})
	}
	lib := newLibrary()
	if _, err := lib.open(func([]byte) error { return nil }); err == nil {
		t.Fatal("unregistered module opened")
	}
	if err := lib.register(fixtureModule(echo)); err != nil {
		t.Fatal(err)
	}
	if err := lib.register(fixtureModule(echo)); !errors.Is(err, errRegistered) {
		t.Fatal("second registration replaced the module:", err)
	}
	if _, err := lib.open(nil); err == nil {
		t.Fatal("missing callback accepted")
	}
}

type panicInbound struct{ codec.Inbound }

func (panicInbound) ID() string { panic("private validation detail") }

type identityInbound struct{ codec.Inbound }

func (identityInbound) ID() string               { return "codec" }
func (identityInbound) Path() string             { return "/native-fixture/rpc" }
func (identityInbound) Contract() codec.Contract { return codec.CatalogContract() }

func TestRegistrationCopiesModule(t *testing.T) {
	module := fixtureModule(echo)
	module.Manifest.Contract.Capabilities = append(module.Manifest.Contract.Capabilities, sdk.ModelDiscovery, sdk.AgentWiring, sdk.InboundCodec)
	module.Manifest.Inbounds = []sdk.InboundDescriptor{{ID: "codec", Path: "/native-fixture/rpc"}}
	module.Inbounds = []codec.Inbound{identityInbound{}}
	module.Manifest.Providers = []sdk.ProviderDescriptor{{
		ID: "echo", Name: "Echo", Protocols: []codec.Protocol{codec.Chat},
		Models: []sdk.Model{{ID: "echo", Name: "Echo", Efforts: []string{"low"},
			Price: &sdk.ModelPrice{Currency: "USD", Input: 1}}},
	}}
	module.Manifest.Agents = []sdk.AgentDescriptor{{ID: "agent", Name: "Agent", Bin: "agent",
		ConfigFiles: []string{".agent/config.json"}, Fields: []sdk.AgentField{{Key: "model", Label: "Model"}}}}
	for _, method := range sdk.RequiredMethods(module.Manifest) {
		if method != sdk.MethodInboundDecode && method != sdk.MethodInboundResponse {
			module.Handlers[method] = echo
		}
	}
	want, _ := json.Marshal(module.Manifest)
	lib := newLibrary()
	if err := lib.register(module); err != nil {
		t.Fatal(err)
	}
	module.Manifest.Contract.Capabilities[0] = sdk.AccountAuth
	module.Manifest.Tools[0].InputSchema[0] = '!'
	module.Manifest.Providers[0].Protocols[0] = codec.Models
	module.Manifest.Providers[0].Models[0].Price.Input = 900
	module.Manifest.Providers[0].Models[0].Efforts[0] = "high"
	module.Manifest.Agents[0].ConfigFiles[0] = ".changed"
	module.Manifest.Agents[0].Fields[0].Label = "Changed"
	module.Inbounds[0] = nil
	module.Handlers[sdk.MethodToolCall] = func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
		return nil, errors.New("replaced handler")
	}
	got, _ := json.Marshal(lib.module.Manifest)
	if !bytes.Equal(got, want) {
		t.Fatalf("registration retained mutable manifest fields:\n%s\n%s", got, want)
	}
	peer := openPeer(t, lib)
	peer.send(call("tool", sdk.MethodToolCall))
	if frame := peer.read(); frame.Error != nil || string(frame.Result) != `{"ok":true}` {
		t.Fatalf("registration retained mutable handler map: %+v", frame)
	}
	peer.send(call("describe", sdk.MethodDescribe))
	var described sdk.Manifest
	frame := peer.read()
	if err := json.Unmarshal(frame.Result, &described); err != nil {
		t.Fatal(err)
	}
	if described.Runtime != "" || described.Entry != "" || len(described.Args) != 0 ||
		!reflect.DeepEqual(described, lib.module.Manifest.Public()) {
		t.Fatalf("native handshake changed the public manifest: %+v", described)
	}
}

func TestOpenWaitsForFirstSendAndCloseRevokesHandle(t *testing.T) {
	lib := newLibrary()
	if err := lib.register(fixtureModule(echo)); err != nil {
		t.Fatal(err)
	}
	var callbacks atomic.Int32
	id, err := lib.open(func([]byte) error { callbacks.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	state := lib.get(id)
	if err := lib.close(id); err != nil {
		t.Fatal(err)
	}
	wait(t, state.done, "unstarted session")
	if callbacks.Load() != 0 {
		t.Fatal("Open emitted before the first Send")
	}
	if lib.send(id, []byte("{}\n")) == nil || lib.close(id) == nil {
		t.Fatal("revoked handle remained usable")
	}
	id2, err := lib.open(func([]byte) error { return nil })
	if err != nil || id2 == id {
		t.Fatal("session handle was reused:", id2, err)
	}
	state2 := lib.get(id2)
	_ = lib.close(id2)
	wait(t, state2.done, "replacement session")
}

func TestSessionsAndConcurrentCallsAreIsolated(t *testing.T) {
	started, release := make(chan struct{}, sdk.MaxConcurrent), make(chan struct{})
	defer close(release)
	module := fixtureModule(func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
		started <- struct{}{}
		select {
		case <-release:
			return call.Scope.Session, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	first := newPeer(t, module)
	second := openPeer(t, first.lib)
	for index := range sdk.MaxConcurrent {
		first.send(call(strconv.Itoa(index), sdk.MethodToolCall))
	}
	for range sdk.MaxConcurrent {
		wait(t, started, "concurrent handler start")
	}
	second.send(call("0", sdk.MethodDescribe))
	if frame := second.read(); frame.ID != "0" || frame.Error != nil {
		t.Fatalf("another session's call ID/worker limit leaked: %+v", frame)
	}
	if err := first.lib.close(first.id); err != nil {
		t.Fatal(err)
	}
	wait(t, first.state.done, "concurrent calls canceled by close")
	second.send(call("after-close", sdk.MethodDescribe))
	if frame := second.read(); frame.ID != "after-close" || frame.Error != nil {
		t.Fatalf("closing one session revoked another: %+v", frame)
	}
}

func TestNativeStreamCreditAndHostCallback(t *testing.T) {
	peer := newPeer(t, fixtureModule(func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
		if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: "native-operation"}); err != nil {
			return nil, err
		}
		var state sdk.StateValue
		err := stream.HostCall(ctx, sdk.MethodStateGet, sdk.StateGetRequest{Key: "echo"}, &state)
		return state, err
	}))
	peer.send(call("stream", sdk.MethodToolCall))
	event := peer.read()
	if event.Kind != sdk.KindEvent || event.Event != "accepted" || event.Seq != 1 {
		t.Fatalf("missing native stream event: %+v", event)
	}
	peer.send(call("independent", sdk.MethodDescribe))
	if frame := peer.read(); frame.ID != "independent" {
		t.Fatalf("stream advanced before acknowledgement: %+v", frame)
	}
	peer.send(sdk.Frame{Kind: sdk.KindAck, ID: event.ID, Seq: event.Seq})
	callback := peer.read()
	if callback.Kind != sdk.KindHostCall || callback.Parent != "stream" ||
		callback.Method != sdk.MethodStateGet || callback.Scope.Session != "stream" {
		t.Fatalf("unscoped native host callback: %+v", callback)
	}
	peer.send(sdk.Frame{Kind: sdk.KindHostResult, ID: callback.ID, Parent: callback.Parent,
		Result: json.RawMessage(`{"found":true,"version":"1","value":{"native":true}}`)})
	if frame := peer.read(); frame.ID != "stream" || frame.Error != nil ||
		!strings.Contains(string(frame.Result), `"native":true`) {
		t.Fatalf("native callback result was lost: %+v", frame)
	}
}

func TestNativeCancellationAndLateReplies(t *testing.T) {
	for _, mode := range []string{"event", "host"} {
		t.Run(mode, func(t *testing.T) {
			peer := newPeer(t, fixtureModule(func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
				if mode == "event" {
					return nil, stream.Emit(ctx, "data", []byte("native"))
				}
				return nil, stream.HostCall(ctx, sdk.MethodStateGet, sdk.StateGetRequest{Key: "echo"}, nil)
			}))
			peer.send(call("cancel", sdk.MethodToolCall))
			pending := peer.read()
			peer.send(sdk.Frame{Kind: sdk.KindCancel, ID: "cancel"})
			if frame := peer.read(); frame.Kind != sdk.KindResult || frame.Error == nil || frame.Error.Code != "canceled" {
				t.Fatalf("native call did not cancel: %+v", frame)
			}
			if mode == "event" {
				peer.send(sdk.Frame{Kind: sdk.KindAck, ID: pending.ID, Seq: pending.Seq})
			} else {
				peer.send(sdk.Frame{Kind: sdk.KindHostResult, ID: pending.ID, Parent: pending.Parent,
					Result: json.RawMessage(`{}`)})
			}
			peer.send(call("healthy", sdk.MethodDescribe))
			if frame := peer.read(); frame.ID != "healthy" || frame.Error != nil {
				t.Fatalf("late reply killed native session: %+v", frame)
			}
		})
	}
}

func TestCloseDoesNotWaitForUncooperativeHandler(t *testing.T) {
	started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	module := fixtureModule(func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
		close(started)
		<-release
		close(returned)
		return nil, nil
	})
	peer := newPeer(t, module)
	defer close(release)
	peer.send(call("stuck", sdk.MethodToolCall))
	wait(t, started, "uncooperative handler start")
	closed := make(chan struct{})
	go func() { _ = peer.lib.close(peer.id); close(closed) }()
	wait(t, closed, "Close with an uncooperative handler")
	select {
	case <-returned:
		t.Fatal("handler fixture did not remain blocked")
	default:
	}
	if peer.lib.get(peer.id) != nil || peer.lib.send(peer.id, []byte("{}\n")) == nil {
		t.Fatal("Close did not immediately revoke the session")
	}
}

func TestCloseUnblocksCallbackAndSend(t *testing.T) {
	t.Run("callback", func(t *testing.T) {
		lib := newLibrary()
		handlerReturned := make(chan struct{})
		if err := lib.register(fixtureModule(func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
			err := stream.Emit(ctx, "data", []byte("blocked"))
			close(handlerReturned)
			return nil, err
		})); err != nil {
			t.Fatal(err)
		}
		started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
		defer func() { close(release); wait(t, returned, "in-flight callback release") }()
		id, err := lib.open(func([]byte) error {
			close(started)
			<-release
			close(returned)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		state := lib.get(id)
		t.Cleanup(func() {
			state.stop()
			wait(t, state.done, "blocked callback session cleanup")
		})
		body, _ := json.Marshal(call("blocked", sdk.MethodToolCall))
		if err := lib.send(id, append(body, '\n')); err != nil {
			t.Fatal(err)
		}
		wait(t, started, "blocked host callback")
		if err := lib.close(id); err != nil {
			t.Fatal(err)
		}
		wait(t, handlerReturned, "Emit canceled while callback blocks")
		wait(t, state.done, "Serve canceled while callback blocks")
		if _, err := state.Write([]byte("{}\n")); err == nil {
			t.Fatal("a future callback started after Close")
		}
	})
	t.Run("send", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		reader, input := io.Pipe()
		state := &session{ctx: ctx, cancel: cancel, reader: reader, input: input,
			started: make(chan struct{}), sendGate: make(chan struct{}, 1)}
		var writers sync.WaitGroup
		for range 2 {
			writers.Add(1)
			go func() {
				defer writers.Done()
				if err := state.send([]byte("{}\n")); err == nil {
					t.Error("blocked Send succeeded without a reader")
				}
			}()
		}
		wait(t, state.started, "blocked Send start")
		state.stop()
		done := make(chan struct{})
		go func() { writers.Wait(); close(done) }()
		wait(t, done, "blocked Sends canceled by Close")
	})
}

func TestFrameBoundsAndCallbackFailures(t *testing.T) {
	peer := newPeer(t, fixtureModule(echo))
	if err := peer.lib.send(peer.id, make([]byte, sdk.MaxFrameBytes+2)); !errors.Is(err, errFrame) {
		t.Fatal("oversized byte span accepted:", err)
	}
	if _, err := peer.state.Write(make([]byte, sdk.MaxFrameBytes+2)); !errors.Is(err, errFrame) {
		t.Fatal("oversized callback accepted:", err)
	}
	peer.send(call("still-open", sdk.MethodDescribe))
	if frame := peer.read(); frame.ID != "still-open" || frame.Error != nil {
		t.Fatalf("rejected span corrupted native session: %+v", frame)
	}
	for _, mode := range []string{"error", "panic", "invalid-frame"} {
		t.Run(mode, func(t *testing.T) {
			lib := newLibrary()
			if err := lib.register(fixtureModule(echo)); err != nil {
				t.Fatal(err)
			}
			id, err := lib.open(func([]byte) error {
				if mode == "panic" {
					panic("private callback detail")
				}
				return errors.New("private callback detail")
			})
			if err != nil {
				t.Fatal(err)
			}
			state := lib.get(id)
			body, _ := json.Marshal(call("failure", sdk.MethodDescribe))
			if mode == "invalid-frame" {
				body = []byte(`{broken`)
			}
			_ = lib.send(id, append(body, '\n'))
			wait(t, state.done, "failed native session")
			if lib.get(id) != nil {
				t.Fatal("failed native session retained a live handle")
			}
		})
	}
}

func TestNativeDeadlineAndHandlerPanic(t *testing.T) {
	for _, mode := range []string{"deadline", "panic"} {
		t.Run(mode, func(t *testing.T) {
			peer := newPeer(t, fixtureModule(func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
				if mode == "panic" {
					panic("private handler detail")
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}))
			frame := call(mode, sdk.MethodToolCall)
			code := "internal"
			if mode == "deadline" {
				frame.Deadline, code = time.Now().Add(-time.Second).UnixMilli(), "deadline"
			}
			peer.send(frame)
			if result := peer.read(); result.Error == nil || result.Error.Code != code ||
				strings.Contains(result.Error.Message, "private") {
				t.Fatalf("native handler error was not bounded: %+v", result)
			}
			peer.send(call("after-error", sdk.MethodDescribe))
			if result := peer.read(); result.ID != "after-error" || result.Error != nil {
				t.Fatalf("handler error killed native session: %+v", result)
			}
		})
	}
}

func TestPublicRegisterMatchesCgoAvailability(t *testing.T) {
	prior := library
	library = newLibrary()
	t.Cleanup(func() { library = prior })
	err := Register(fixtureModule(echo))
	if !available {
		if !errors.Is(err, ErrUnavailable) {
			t.Fatal("non-cgo registration did not explain native unavailability:", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(fixtureModule(echo)); !errors.Is(err, errRegistered) {
		t.Fatal("public Register allowed replacement:", err)
	}
}

func TestNativeCompositeSnapshotRetainsRootActionsAndPerSessionStartup(t *testing.T) {
	type ComponentState struct {
		Version string
		Opaque  chan struct{}
	}
	var starts, disposals atomic.Int32
	module := fixtureModule(echo)
	module.Manifest.Contract.Version.Minor = 1
	module.Manifest.Contract.Capabilities = append(module.Manifest.Contract.Capabilities, sdk.ModuleRPC)
	module.Manifest.Actions = []sdk.ActionDescriptor{{ID: "config", Effect: "read", InputSchema: json.RawMessage(`true`)}}
	module.Manifest.Services = []sdk.ServiceDescriptor{{
		ID: "helper", Runtime: "exec", Entry: "bin/helper", Start: "lazy",
		Contract: sdk.ServiceContract{ID: "helper", Version: 1},
	}}
	module.Actions = map[string]sdk.Handler{"config": func(ctx context.Context, _ sdk.Call, _ *sdk.Stream) (any, error) {
		value, found := sdk.ModuleContextFrom(ctx)
		if !found {
			return nil, errors.New("missing module context")
		}
		state, found := value.State.(*ComponentState)
		if !found || state.Version != value.Config.Version {
			return nil, errors.New("missing or foreign native instance state")
		}
		return state.Version, nil
	}}
	module.Start = func(_ context.Context, value sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
		starts.Add(1)
		state := &ComponentState{Version: value.Config.Version, Opaque: make(chan struct{})}
		return sdk.ModuleReady{State: state}, func() { close(state.Opaque); disposals.Add(1) }, nil
	}
	lib := newLibrary()
	if err := lib.register(module); err != nil {
		t.Fatal(err)
	}
	module.Manifest.Services[0].Entry = "changed"
	module.Actions["config"] = echo
	module.Start = nil
	if len(lib.module.Manifest.Services) != 1 || lib.module.Manifest.Services[0].Entry != "bin/helper" {
		t.Fatal("native registration used the public projection as root authority")
	}
	for _, version := range []string{"1", "2"} {
		peer := openPeer(t, lib)
		frame := call("configure", sdk.MethodComponentConfigure)
		frame.Params, _ = json.Marshal(sdk.ComponentConfigureRequest{
			Component: sdk.PrimaryComponent, Instance: "instance-" + version,
			Config: sdk.ConfigSnapshot{Version: version, Values: json.RawMessage(`{}`)},
		})
		peer.send(frame)
		if result := peer.read(); result.Error != nil {
			t.Fatal("native component did not configure:", result)
		}
		frame = call("action", sdk.MethodModuleCall)
		frame.Params, _ = json.Marshal(sdk.ModuleCallRequest{Action: "config", Input: json.RawMessage(`{}`)})
		peer.send(frame)
		if result := peer.read(); result.Error != nil || string(result.Result) != `"`+version+`"` {
			t.Fatal("native action map or session configuration was not frozen:", result)
		}
		_ = lib.close(peer.id)
		wait(t, peer.state.done, "native composite disposal")
	}
	if starts.Load() != 2 || disposals.Load() != 2 {
		t.Fatal("native sessions shared startup or leaked disposers:", starts.Load(), disposals.Load())
	}
}
