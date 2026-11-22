package extension

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"testing/synctest"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSDKMiddlewareRevokedWhileWaitingForWorker(t *testing.T) {
	manager := NewManager(t.TempDir())
	peer := NewManager(manager.Namespace())
	source := catalogTestPackage(t, "middleware.fixture", "1.0.0")
	descriptor := &sdk.GatewayMiddlewareDescriptor{Event: true}
	catalogTestWriteManifest(t, source, sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: "middleware.fixture", Version: "1.0.0",
		Runtime: "exec", Entry: "bin/worker", Middleware: descriptor,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8},
			Capabilities: []sdk.Capability{sdk.GatewayMiddleware}},
	})
	entry := catalogTestInstall(t, manager.catalog, source)
	synctest.Test(t, func(t *testing.T) {
		runs := manager.BeginMiddleware(t.Context(), sdk.GatewayContext{Origin: "local", Stream: true})
		if len(runs) != 1 {
			t.Fatal("expected one middleware run", len(runs))
		}
		run := runs[0]
		defer run.Close()
		connection, wire := net.Pipe()
		lifetime, cancel := context.WithCancel(run.ctx)
		client := &workerClient{
			stdin: connection, stdout: connection, cancel: cancel, lifetime: lifetime,
			done: make(chan struct{}), readDone: make(chan struct{}), writeDone: make(chan struct{}),
			pending: make(map[string]*pendingCall), slots: make(chan struct{}, 1), regularSlots: make(chan struct{}, 1),
			writes: make(chan workerWrite), cancelFrames: make(chan sdk.Frame, 1),
		}
		go client.read()
		go client.writeLoop()
		calls := make(chan sdk.Frame, 1)
		wireDone := make(chan struct{})
		go func() {
			defer close(wireDone)
			var frame sdk.Frame
			if json.NewDecoder(wire).Decode(&frame) == nil {
				calls <- frame
				_ = json.NewEncoder(wire).Encode(sdk.Frame{
					Kind: "result", ID: frame.ID, Result: json.RawMessage(`{"state":{"stale":true}}`),
				})
			}
		}()
		t.Cleanup(func() {
			client.fail(io.EOF)
			wire.Close()
			<-wireDone
			<-client.readDone
			<-client.writeDone
			client.cancelWG.Wait()
		})
		leaseContext, release := context.WithCancel(run.ctx)
		run.lease = &Lease{manager: manager, worker: &managedWorker{
			entry: entry, active: 1, idle: make(chan struct{}),
		}, client: client, ctx: leaseContext, cancel: release}
		client.regularSlots <- struct{}{}
		result := make(chan error, 1)
		go func() {
			_, err := run.Apply(sdk.MethodMiddlewareEvent, []byte(`{"delta":"text"}`), sdk.MiddlewareContext{Event: "delta"})
			result <- err
		}()
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatal("hook returned before the worker slot opened", err)
		default:
		}
		if _, err := peer.CompareAndSwapModuleConfig(t.Context(), entry.Manifest.ID, run.config.Version, json.RawMessage(`{"updated":true}`)); err != nil {
			t.Fatal(err)
		}
		<-client.regularSlots
		if err := <-result; !errors.Is(err, errMiddlewareChanged) {
			t.Fatal("hook did not report revocation", err)
		}
		select {
		case frame := <-calls:
			t.Fatal("revoked middleware reached the worker after waiting for its slot", frame.Method)
		default:
		}
		if len(run.state) != 0 {
			t.Fatal("revoked middleware changed state", string(run.state))
		}
	})
}
