package extension

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestOwnedProcessParentReturnJoinsAdmittedHostCallbacks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		connection, peer := net.Pipe()
		lifetime, stop := context.WithCancel(t.Context())
		client := &workerClient{
			stdin: connection, stdout: connection, cancel: stop, lifetime: lifetime,
			done: make(chan struct{}), readDone: make(chan struct{}), writeDone: make(chan struct{}),
			pending: map[string]*pendingCall{}, slots: make(chan struct{}, 4), regularSlots: make(chan struct{}, 4),
			writes: make(chan workerWrite), cancelFrames: make(chan sdk.Frame, 4),
		}
		entered, canceled, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		client.hostCall = func(ctx context.Context, _ sdk.Scope, _ string, _ json.RawMessage) (any, error) {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			close(exited)
			return nil, ctx.Err()
		}
		go client.read()
		go client.writeLoop()
		t.Cleanup(func() {
			once.Do(func() { close(release) })
			client.fail(io.EOF)
			peer.Close()
			<-client.readDone
			<-client.writeDone
			client.hostWG.Wait()
			client.cancelWG.Wait()
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		scope := sdk.Scope{Plugin: "fixture", Operation: "owned", Attempt: 1}
		done := make(chan error, 1)
		returned := make(chan struct{})
		go func() {
			_, err := client.call(ctx, sdk.MethodModuleCall, scope, nil, struct{}{}, nil)
			done <- err
			close(returned)
		}()
		var request sdk.Frame
		if err := json.NewDecoder(peer).Decode(&request); err != nil {
			t.Fatal(err)
		}
		params := json.RawMessage(`{"handle":"opaque","pipe":"stdout","seq":"1","maxBytes":64}`)
		if err := json.NewEncoder(peer).Encode(sdk.Frame{
			Kind: sdk.KindHostCall, ID: "read-1", Parent: request.ID, Scope: scope,
			Method: sdk.MethodProcessRead, Params: params,
		}); err != nil {
			t.Fatal(err)
		}
		<-entered
		cancel()
		<-canceled
		synctest.Wait()
		select {
		case <-returned:
			t.Error("parent returned while an admitted host callback still owned its cleanup barrier")
		default:
		}
		once.Do(func() { close(release) })
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("canceled parent lost its actual terminal", err)
		}
		select {
		case <-exited:
		default:
			t.Fatal("parent returned before its callback cleanup joined")
		}
	})
}

func TestOwnedProcessTerminalClosesIOBeforeJoiningCallbacks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(t.TempDir())
		manager.diagnosticLog = func(moduleDiagnostic) {}
		scope := sdk.Scope{Plugin: "fixture", Operation: "owned-terminal", Attempt: 1}
		ownerCtx, cancelOwner := context.WithCancel(t.Context())
		defer cancelOwner()
		ioCtx, stopIO := context.WithCancel(ownerCtx)
		parent := &ownedProcess{
			lease: &Lease{manager: manager}, scope: scope, ctx: ownerCtx, cancel: cancelOwner,
			ioctx: ioCtx, stopIO: stopIO, bound: true, component: sdk.PrimaryComponent,
			started: time.Now(), closeRequest: make(chan struct{}), closed: make(chan struct{}),
		}
		go parent.shutdown()
		connection, peer := net.Pipe()
		lifetime, stop := context.WithCancel(t.Context())
		client := &workerClient{
			stdin: connection, stdout: connection, cancel: stop, lifetime: lifetime,
			done: make(chan struct{}), readDone: make(chan struct{}), writeDone: make(chan struct{}),
			pending: map[string]*pendingCall{}, slots: make(chan struct{}, 4), regularSlots: make(chan struct{}, 4),
			writes: make(chan workerWrite), cancelFrames: make(chan sdk.Frame, 4),
		}
		entered, canceled, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
		client.hostCall = func(ctx context.Context, _ sdk.Scope, _ string, _ json.RawMessage) (any, error) {
			if err := parent.beginIO(); err != nil {
				return nil, err
			}
			defer parent.ioWG.Done()
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-parent.ioctx.Done()
			close(exited)
			return nil, ctx.Err()
		}
		go client.read()
		go client.writeLoop()
		done, returned := make(chan error, 1), make(chan struct{})
		t.Cleanup(func() {
			parent.close("test-cleanup")
			client.fail(io.EOF)
			peer.Close()
			<-client.readDone
			<-client.writeDone
			client.hostWG.Wait()
			client.cancelWG.Wait()
			<-returned
		})
		ctx := context.WithValue(t.Context(), processContextKey{}, parent)
		go func() {
			_, err := client.call(ctx, sdk.MethodModuleCall, scope, nil, struct{}{}, nil)
			done <- err
			close(returned)
		}()
		var request sdk.Frame
		if err := json.NewDecoder(peer).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(peer).Encode(sdk.Frame{
			Kind: sdk.KindHostCall, ID: "write-1", Parent: request.ID, Scope: scope,
			Method: sdk.MethodProcessWrite,
			Params: json.RawMessage(`{"handle":"opaque","writeId":"write-1","seq":"1","bytes":"eA=="}`),
		}); err != nil {
			t.Fatal(err)
		}
		<-entered
		if err := json.NewEncoder(peer).Encode(sdk.Frame{
			Kind: sdk.KindResult, ID: request.ID,
			Error: &sdk.Error{Code: "synthetic_terminal", Message: "synthetic terminal"},
		}); err != nil {
			t.Fatal(err)
		}
		<-canceled
		synctest.Wait()
		select {
		case <-returned:
		default:
			t.Error("terminal error waited for callback before revoking owned process I/O")
			parent.close("test-cleanup")
		}
		var terminal *sdk.Error
		if err := <-done; !errors.As(err, &terminal) || terminal.Code != "synthetic_terminal" {
			t.Fatal("owned process cleanup lost the actual terminal", err)
		}
		select {
		case <-exited:
		default:
			t.Fatal("terminal returned before its owned I/O callback joined")
		}
		select {
		case <-parent.closed:
		default:
			t.Fatal("terminal returned before its process owner joined")
		}
	})
}

type ownedProcessReplyBarrier struct {
	io.WriteCloser
	delivered chan struct{}
	release   chan struct{}
}

func (writer *ownedProcessReplyBarrier) Write(body []byte) (int, error) {
	count, err := writer.WriteCloser.Write(body)
	var frame sdk.Frame
	if err == nil && json.Unmarshal(body, &frame) == nil && frame.Kind == sdk.KindHostResult {
		close(writer.delivered)
		<-writer.release
	}
	return count, err
}

func TestOwnedProcessSuccessfulTerminalJoinsDeliveredReplyAndKeepsWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(t.TempDir())
		manager.diagnosticLog = func(moduleDiagnostic) {}
		scope := sdk.Scope{Plugin: "fixture", Operation: "owned-success", Attempt: 1}
		ownerCtx, cancelOwner := context.WithCancel(t.Context())
		defer cancelOwner()
		ioCtx, stopIO := context.WithCancel(ownerCtx)
		parent := &ownedProcess{
			lease: &Lease{manager: manager}, scope: scope, ctx: ownerCtx, cancel: cancelOwner,
			ioctx: ioCtx, stopIO: stopIO, bound: true, component: sdk.PrimaryComponent,
			started: time.Now(), closeRequest: make(chan struct{}), closed: make(chan struct{}),
		}
		go parent.shutdown()
		connection, peer := net.Pipe()
		writer := &ownedProcessReplyBarrier{
			WriteCloser: connection, delivered: make(chan struct{}), release: make(chan struct{}),
		}
		lifetime, stop := context.WithCancel(t.Context())
		client := &workerClient{
			stdin: writer, stdout: connection, cancel: stop, lifetime: lifetime,
			done: make(chan struct{}), readDone: make(chan struct{}), writeDone: make(chan struct{}),
			pending: map[string]*pendingCall{}, slots: make(chan struct{}, 4), regularSlots: make(chan struct{}, 4),
			writes: make(chan workerWrite), cancelFrames: make(chan sdk.Frame, 4),
			hostCall: func(context.Context, sdk.Scope, string, json.RawMessage) (any, error) {
				return struct{}{}, nil
			},
		}
		go client.read()
		go client.writeLoop()
		done, returned := make(chan error, 1), make(chan struct{})
		var release sync.Once
		var nextReturned <-chan struct{}
		t.Cleanup(func() {
			release.Do(func() { close(writer.release) })
			parent.close("test-cleanup")
			client.fail(io.EOF)
			peer.Close()
			<-client.readDone
			<-client.writeDone
			client.hostWG.Wait()
			client.cancelWG.Wait()
			<-returned
			if nextReturned != nil {
				<-nextReturned
			}
		})
		ctx := context.WithValue(t.Context(), processContextKey{}, parent)
		go func() {
			_, err := client.call(ctx, sdk.MethodModuleCall, scope, nil, struct{}{}, nil)
			done <- err
			close(returned)
		}()
		decoder, encoder := json.NewDecoder(peer), json.NewEncoder(peer)
		var request sdk.Frame
		if err := decoder.Decode(&request); err != nil {
			t.Fatal(err)
		}
		if err := encoder.Encode(sdk.Frame{
			Kind: sdk.KindHostCall, ID: "open-1", Parent: request.ID, Scope: scope,
			Method: sdk.MethodProcessOpen, Params: json.RawMessage(`{"recipeId":"run"}`),
		}); err != nil {
			t.Fatal(err)
		}
		var reply sdk.Frame
		if err := decoder.Decode(&reply); err != nil || reply.Kind != sdk.KindHostResult || reply.Error != nil {
			t.Fatal("host reply was not delivered", reply, err)
		}
		<-writer.delivered
		if err := encoder.Encode(sdk.Frame{Kind: sdk.KindResult, ID: request.ID, Result: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if !client.alive() {
			t.Error("successful terminal canceled an already delivered host reply and retired its worker")
		}
		select {
		case <-returned:
			t.Error("successful terminal abandoned its host reply before its transport joined")
		default:
		}
		release.Do(func() { close(writer.release) })
		if err := <-done; err != nil {
			t.Fatal("successful terminal lost its actual result", err)
		}
		if !client.alive() {
			return
		}
		next := make(chan error, 1)
		nextDone := make(chan struct{})
		nextReturned = nextDone
		go func() {
			defer close(nextDone)
			_, err := client.call(t.Context(), sdk.MethodModuleCall, scope, nil, struct{}{}, nil)
			next <- err
		}()
		if err := decoder.Decode(&request); err != nil {
			t.Fatal("next independent call lost the healthy worker", err)
		}
		if err := encoder.Encode(sdk.Frame{Kind: sdk.KindResult, ID: request.ID, Result: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if err := <-next; err != nil {
			t.Fatal("next independent call failed after completed host reply", err)
		}
	})
}
