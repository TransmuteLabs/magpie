package extension

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestWindowAuthReloadDoesNotBlockUnrelatedModels(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "auth.trace")
	manager, authID, revision := authWindowFixture(t, false, authfixture.Config{Trace: trace})
	modelsEntry, err := manager.Install(t.Context(), fixturePackage(t))
	if err != nil {
		t.Fatal(err)
	}
	readModels := func(ctx context.Context) error {
		var result sdk.ModelsResult
		if err := manager.Invoke(ctx, modelsEntry.Manifest.ID, sdk.MethodProviderModels, sdk.Scope{Provider: "provider"}, nil, &result, nil); err != nil {
			return err
		}
		if len(result.Models) != 1 || result.Models[0].ID != "model" {
			return errors.New("unrelated worker did not return its model")
		}
		return nil
	}
	if err := readModels(t.Context()); err != nil {
		t.Fatalf("warm unrelated worker: %v", err)
	}

	const part, binding = "window:page", "signin"
	options := sdk.WindowAuthStartOptions{Method: "device"}
	authCtx := authWindowContext(t.Context(), "revoked-intent")
	started, err := manager.CoreAuthStart(authCtx, authID, revision, part, binding, options)
	if err != nil || started.State != "waiting" {
		t.Fatalf("start auth: %s %v", started.State, err)
	}
	authWindowReady(t, manager, authID, started)

	peer := NewManager(manager.root)
	closeActivationTestManager(t, peer)
	if err := peer.SetEnabled(t.Context(), authID, false); err != nil {
		t.Fatalf("disable auth module through peer: %v", err)
	}
	lock, err := os.OpenFile(filepath.Join(manager.root, "private", ".lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if held, err := filelock.TryShared(lock); err != nil || !held {
		t.Fatalf("hold the peer Store reader: held=%v %v", held, err)
	}
	unlock := sync.OnceFunc(func() { filelock.Unlock(lock) })

	reloaded := make(chan struct{})
	var reloadErr error
	go func() {
		defer close(reloaded)
		reloadErr = manager.Reload(t.Context())
	}()
	defer func() {
		unlock()
		<-reloaded
	}()
	waitActivation(t, "window auth revocation's exclusive Store wait", func() bool {
		select {
		case <-reloaded:
			t.Fatalf("reload returned while Store revocation was blocked: %v", reloadErr)
		default:
		}
		if manager.store.mu.TryLock() {
			manager.store.mu.Unlock()
			return false
		}
		return true
	})

	modelsCtx, stopModels := context.WithTimeout(t.Context(), 2*time.Second)
	err = readModels(modelsCtx)
	stopModels()
	if err != nil {
		t.Errorf("unrelated provider.models did not complete while the auth revocation waited for Store: %v", err)
	}

	if err := peer.SetEnabled(t.Context(), authID, true); err != nil {
		t.Fatalf("re-enable auth module while the old flow is being revoked: %v", err)
	}
	finishCtx, stopFinish := context.WithTimeout(authCtx, time.Second)
	old, err := manager.CoreAuthFinish(finishCtx, authID, revision, part, binding, started.Flow)
	stopFinish()
	if err == nil && old.State != "canceled" && old.State != "uncertain" {
		t.Errorf("revoked flow remained usable after re-enable: %s", old.State)
	}
	if calls := authWindowTrace(t, trace, "finish"); calls != 0 {
		t.Errorf("revoked flow dispatched while Store was locked: %d calls", calls)
	}

	unlock()
	<-reloaded
	if reloadErr != nil {
		t.Fatalf("reload after Store unlock: %v", reloadErr)
	}
	record, err := manager.store.readWindowAuthRecord(t.Context(), authID, started.Flow, false)
	if err != nil || record.Phase != "canceled" || !record.Canceled || len(record.Continuation) != 0 {
		t.Fatalf("revocation was not durable: phase=%s canceled=%v continuation=%d bytes error=%v",
			record.Phase, record.Canceled, len(record.Continuation), err)
	}
	replayed, err := manager.CoreAuthFinish(authCtx, authID, revision, part, binding, started.Flow)
	if err != nil || replayed.State != "canceled" || authWindowTrace(t, trace, "finish") != 0 {
		t.Fatalf("canceled flow did not replay locally: %s %v", replayed.State, err)
	}

	freshCtx := authWindowContext(t.Context(), "fresh-intent")
	fresh, err := manager.CoreAuthStart(freshCtx, authID, revision, part, binding, options)
	if err != nil || fresh.State != "waiting" || fresh.Flow == started.Flow {
		t.Fatalf("new flow after re-enable: %s %v", fresh.State, err)
	}
	authWindowReady(t, manager, authID, fresh)
	complete, err := manager.CoreAuthFinish(freshCtx, authID, revision, part, binding, fresh.Flow)
	if err != nil || complete.State != "complete" || complete.Account == nil {
		t.Fatalf("complete new flow after re-enable: %s %v", complete.State, err)
	}
	replayed, err = manager.CoreAuthFinish(freshCtx, authID, revision, part, binding, fresh.Flow)
	if err != nil || replayed.State != "complete" || replayed.Account == nil || *replayed.Account != *complete.Account ||
		authWindowTrace(t, trace, "start") != 2 || authWindowTrace(t, trace, "finish") != 1 {
		t.Fatalf("replaying the confirmed flow changed the receipt or repeated a worker call: %s %v", replayed.State, err)
	}
}
