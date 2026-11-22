package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func installActivationFixture(t *testing.T, manager *Manager, id, describeGate string) {
	t.Helper()
	source := activationFixturePackage(t)
	raw, err := os.ReadFile(filepath.Join(source, sdk.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.ID = id
	if describeGate != "" {
		manifest.Args = append(manifest.Args, "--sdk-activation-describe-gate", describeGate)
	}
	catalogTestWriteManifest(t, source, manifest)
	if _, err := manager.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayActivationStartsAnotherModuleWhileRetiring(t *testing.T) {
	const first, second = "activation-first", "activation-second"
	manager := NewManager(t.TempDir())
	closeActivationTestManager(t, manager)
	installActivationFixture(t, manager, first, "")
	installActivationFixture(t, manager, second, "")
	peer := NewCatalog(manager.Namespace())
	if err := peer.SetEnabled(t.Context(), second, false); err != nil {
		t.Fatal(err)
	}
	owner, err := manager.BeginGatewayActivation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	waitActivation(t, "first gateway activation", func() bool { return manager.Running(first) })
	held, err := manager.Acquire(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	waitActivation(t, "both the listener and request hold the first module", func() bool {
		held.worker.mu.Lock()
		defer held.worker.mu.Unlock()
		return held.worker.active == 2
	})
	if err := peer.SetEnabled(t.Context(), first, false); err != nil {
		t.Fatal(err)
	}
	owner.wake()
	waitActivation(t, "the first module is detached", func() bool {
		return !manager.Running(first)
	})
	if fresh, err := manager.Acquire(t.Context(), first); err == nil {
		fresh.Close()
		t.Fatal("disabled module accepted a new request while draining")
	}
	if err := peer.SetEnabled(t.Context(), second, true); err != nil {
		t.Fatal(err)
	}
	owner.wake()
	waitIndependentActivation(t, manager, second)
	if !held.client.alive() {
		t.Fatal("the second module only activated after the first stopped")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := owner.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("listener ownership released before the first worker drained: %v", err)
	}
	held.Close()
	ctxJoin, cancelJoin := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelJoin()
	if err := owner.Close(ctxJoin); err != nil {
		t.Fatalf("listener did not join after the first worker drained: %v", err)
	}
	select {
	case <-held.client.done:
	default:
		t.Fatal("listener closed with the old worker still running")
	}
}

func TestGatewayActivationStartsAnotherModuleDuringHandshake(t *testing.T) {
	const first, second = "activation-first", "activation-second"
	manager := NewManager(t.TempDir())
	closeActivationTestManager(t, manager)
	gate := filepath.Join(t.TempDir(), "describe")
	installActivationFixture(t, manager, first, gate)
	installActivationFixture(t, manager, second, "")
	peer := NewCatalog(manager.Namespace())
	if err := peer.SetEnabled(t.Context(), second, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(gate)
	owner, err := manager.BeginGatewayActivation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	waitActivation(t, "first worker reached its describe barrier", func() bool {
		_, err := os.Stat(gate + ".entered")
		return err == nil
	})
	if err := peer.SetEnabled(t.Context(), second, true); err != nil {
		t.Fatal(err)
	}
	owner.wake()
	waitIndependentActivation(t, manager, second)
	if manager.Running(first) {
		t.Fatal("held first worker completed its handshake")
	}
	if err := peer.SetEnabled(t.Context(), first, false); err != nil {
		t.Fatal(err)
	}
	owner.wake()
	waitActivation(t, "revoked start is detached", func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return manager.workers[first] == nil
	})
	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetEnabled(t.Context(), first, true); err != nil {
		t.Fatal(err)
	}
	owner.wake()
	waitActivation(t, "new first generation after canceled handshake", func() bool {
		return manager.Running(first)
	})
}

func TestGatewayActivationLateResultKeepsReplacementClient(t *testing.T) {
	for _, test := range []struct {
		name      string
		handshake bool
	}{
		{name: "running"},
		{name: "handshaking", handshake: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const id = "activation-replaced"
			manager := NewManager(t.TempDir())
			closeActivationTestManager(t, manager)
			gate := filepath.Join(t.TempDir(), "describe")
			installActivationFixture(t, manager, id, gate)
			owner := activationTestOwner(t, manager)
			watch := newActivationWatch(manager.root)
			defer watch.close()
			owner.reconcile(watch)

			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			var result activationResult
			select {
			case result = <-owner.started:
			case <-ctx.Done():
				t.Fatal("first client did not reach the activation result queue")
			}
			queued := true
			defer func() {
				if queued {
					owner.finishStart(result)
				}
			}()
			if result.err != nil || result.lease == nil || !result.lease.client.alive() {
				t.Fatalf("first start did not queue a live client: %v", result.err)
			}
			stopActivationWorker(t, result.lease.client)

			if test.handshake {
				if err := os.WriteFile(gate, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(gate)
			}
			var replacement *Lease
			var acquireErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				replacement, acquireErr = manager.AcquireRevision(ctx, id, result.lease.Revision(), result.lease.ConfigVersion())
			}()
			defer func() {
				cancel()
				<-done
				if replacement != nil {
					replacement.Close()
				}
			}()
			if test.handshake {
				waitActivation(t, "replacement handshake", func() bool {
					_, err := os.Stat(gate + ".entered")
					return err == nil
				})
				worker := result.lease.worker
				worker.mu.Lock()
				starting := worker.starting != nil && worker.client == nil
				worker.mu.Unlock()
				if !starting {
					t.Fatal("ordinary acquire did not start a handshake for the same worker generation")
				}
			} else {
				<-done
				if acquireErr != nil || !manager.Running(id) || replacement.worker != result.lease.worker ||
					replacement.client == result.lease.client {
					t.Fatalf("ordinary acquire did not restart the same worker generation: %v", acquireErr)
				}
			}
			owner.finishStart(result)
			queued = false
			if test.handshake {
				if err := os.Remove(gate); err != nil {
					t.Fatal(err)
				}
			}
			<-done
			if acquireErr != nil {
				t.Fatalf("queued activation result retired the replacement during its handshake: %v", acquireErr)
			}
			if !manager.Running(id) {
				t.Fatal("queued activation result detached the running replacement client")
			}
			fresh, err := manager.AcquireRevision(ctx, id, replacement.Revision(), replacement.ConfigVersion())
			if err != nil {
				t.Fatal(err)
			}
			defer fresh.Close()
			if fresh.client != replacement.client || fresh.worker != result.lease.worker {
				t.Fatal("ordinary acquire did not reuse the replacement client")
			}
		})
	}
}

func waitIndependentActivation(t *testing.T, manager *Manager, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !manager.Running(id) {
		select {
		case <-ctx.Done():
			t.Fatalf("independent module %s did not activate while the first was held", id)
		case <-ticker.C:
		}
	}
}
