package extension

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

// Isolate the deadlock negative: the parent joins the child even when neither
// owner can observe cancellation. No external worker is started by this fixture.
func TestRestartAndRetainedRefreshDoNotDeadlock(t *testing.T) {
	const child = "MAGPIE_TEST_RESTART_REFRESH_CHILD"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRestartAndRetainedRefreshDoNotDeadlock$", "-test.count=1")
		cmd.Env = append(os.Environ(), child+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("restart and retained refresh must finish after cancellation: %v (%v)\n%s", err, ctx.Err(), output)
		}
		if !strings.Contains(string(output), "restart-refresh-completed") {
			t.Fatalf("child did not exercise restart/refresh: %s", output)
		}
		return
	}
	manifest := sdk.Manifest{ID: "restart-fixture", Contract: sdk.Contract{
		Version: sdk.FullContract().Version, Capabilities: []sdk.Capability{sdk.AccountAuth},
	}, Providers: []sdk.ProviderDescriptor{{ID: "provider", Name: "Provider"}}}
	manager := NewManager(t.TempDir())
	manager.mu.Lock()
	manager.startLifecycleLocked()
	manager.mu.Unlock()
	done := make(chan struct{})
	close(done)
	client := &workerClient{
		err: errors.New("fixture transport stopped"), cancel: func() {}, done: done,
		stdin: restartDiscardCloser{}, stdout: io.NopCloser(strings.NewReader("")),
	}
	worker := &managedWorker{
		entry: Installed{Manifest: manifest, Revision: "fixture"}, config: sdk.ConfigSnapshot{Version: "0"},
		client: client, active: 1, idle: make(chan struct{}),
	}
	manager.workers[manifest.ID] = worker
	lease := &Lease{manager: manager, worker: worker, client: client, ctx: context.Background(), cancel: func() {}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// alive must wait here while acquisition retains the worker lock. Refresh
	// then retains the manager lock on the broken code: an actual ABBA cycle.
	client.mu.Lock()
	restarted, refreshed := make(chan struct{}), make(chan struct{})
	go func() {
		_, _, _ = manager.acquireSelected(ctx, manifest.ID, &worker.entry, worker.config)
		close(restarted)
	}()
	waitRestartMutexHeld(t, &worker.mu)
	go func() {
		_, _ = manager.refreshFromLease(ctx, lease, sdk.Scope{Plugin: manifest.ID, Provider: "provider", Account: "account", Generation: 1})
		close(refreshed)
	}()
	waitRestartMutexHeld(t, &manager.mu)
	cancel()
	client.mu.Unlock()
	<-restarted
	<-refreshed
	lease.Close()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if err := manager.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(os.Stdout, "restart-refresh-completed\n")
}

func waitRestartMutexHeld(t *testing.T, mutex *sync.Mutex) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for mutex.TryLock() {
		mutex.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("fixture did not reach the held mutex")
		}
		runtime.Gosched()
	}
}

type restartDiscardCloser struct{}

func (restartDiscardCloser) Write(p []byte) (int, error) { return len(p), nil }
func (restartDiscardCloser) Close() error                { return nil }
