package extension

import (
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

type resultWriter func([]byte) (int, error)

func (writer resultWriter) Write(body []byte) (int, error) { return writer(body) }

func TestTerminalResultRetiresItsIDBeforePeerCanReuseIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	serverContext, stop := context.WithCancelCause(ctx)
	defer stop(context.Canceled)
	started, release := make(chan struct{}), make(chan struct{})
	server := &worker{ctx: serverContext, cancel: stop, manifest: operationModule(testNoop).Manifest,
		output: make(chan outputFrame, MaxConcurrent), calls: map[string]*callState{}, host: map[string]*hostWait{},
		handlers: map[string]Handler{MethodOperationStatus: func(ctx context.Context, call Call, _ *Stream) (any, error) {
			if string(call.Params) == "true" {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return true, nil
		}}}
	callContext, endCall := context.WithCancel(ctx)
	original := &callState{call: Call{ID: "reused", Method: MethodOperationStatus}, ctx: callContext, cancel: endCall}
	server.calls["reused"] = original
	observed := make(chan error, 1)
	var writes atomic.Int32
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		server.writeLoop(resultWriter(func(body []byte) (int, error) {
			if writes.Add(1) == 1 {
				var frame Frame
				if err := json.Unmarshal(body, &frame); err != nil {
					observed <- err
					return 0, err
				}
				if frame.Kind != KindResult {
					observed <- io.ErrUnexpectedEOF
					return 0, io.ErrUnexpectedEOF
				}
				observed <- server.dispatch(callFrame("reused", MethodOperationStatus, true))
			}
			return len(body), nil
		}))
	}()
	oldDone := make(chan struct{})
	server.running.Add(1)
	go func() { defer close(oldDone); server.run(original) }()
	t.Cleanup(func() {
		stop(context.Canceled)
		server.running.Wait()
		<-writerDone
	})
	select {
	case err := <-observed:
		if err != nil {
			t.Fatalf("a peer receiving the terminal result cannot reuse its request ID: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("terminal result was not written")
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("the reused request did not start")
	}
	select {
	case <-oldDone:
	case <-ctx.Done():
		t.Fatal("the completed request did not finish")
	}
	server.mu.Lock()
	retained := server.calls["reused"]
	server.mu.Unlock()
	if retained == nil || retained == original {
		t.Fatal("cleanup of the completed call removed the newer call with the same ID")
	}
	close(release)
	server.running.Wait()
}
