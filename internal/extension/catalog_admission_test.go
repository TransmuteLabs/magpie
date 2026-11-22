package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestReloadWithdrawsUnreadableCatalogGeneration(t *testing.T) {
	manager, installed := fixtureManager(t)
	lease, err := manager.Acquire(t.Context(), installed.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	filename := filepath.Join(manager.Namespace(), "catalog.json")
	original, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := manager.Reload(ctx); !errors.Is(err, context.Canceled) || !manager.Running(installed.Manifest.ID) {
		t.Fatalf("cancellation withdrew a healthy worker: %v", err)
	}
	catalogTestWrite(t, filename, []byte("["), 0o600)
	if err := manager.Reload(t.Context()); err == nil || manager.Running(installed.Manifest.ID) {
		t.Fatalf("unreadable catalog left its worker available: %v", err)
	}
	if rejected, err := manager.Acquire(t.Context(), installed.Manifest.ID); err == nil {
		rejected.Close()
		t.Fatal("unreadable catalog admitted a lease")
	}
	catalogTestWrite(t, filename, original, 0o600)
	repaired, err := manager.Acquire(t.Context(), installed.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer repaired.Close()
	if repaired.worker == lease.worker {
		t.Fatal("repair reused a withdrawn worker")
	}
}

func TestOutsideCallsPublicationRequiresStableStoreLock(t *testing.T) {
	manager, installed := fixtureManager(t)
	scope := sdk.Scope{Plugin: installed.Manifest.ID, Provider: "provider", Account: "reader"}
	account, err := manager.Store().SaveAccount(Account{
		Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account, Auth: json.RawMessage(`{"key":"fixture"}`),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope.Generation = account.Generation
	ctx := WithSubscriptionOwnerRevision(t.Context(), "")
	entry, config, err := manager.ActiveModuleConfig(ctx, scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(manager.Namespace(), "private", ".lock")
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	published := 0
	publish := func() error {
		peer, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		defer peer.Close()
		if held, err := filelock.TryLock(peer); err != nil {
			return err
		} else if held {
			filelock.Unlock(peer)
			t.Error("a competing writer could enter during outside-call publication")
		}
		published++
		return nil
	}
	if err := manager.PublishOutsideCalls(ctx, entry, config.Version, scope, publish); err != nil || published != 1 {
		t.Fatalf("outside calls did not publish once under a restored store lock: count %d, error %v", published, err)
	}
	account, err = NewStore(manager.Namespace()).SaveAccount(account, account.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.PublishOutsideCalls(ctx, entry, config.Version, scope, publish); err == nil || published != 1 {
		t.Fatalf("outside calls published for a revoked generation: count %d, error %v", published, err)
	}
	scope.Generation = account.Generation
	if err := manager.PublishOutsideCalls(ctx, entry, config.Version, scope, publish); err != nil || published != 2 {
		t.Fatalf("outside calls did not publish once under the store lock: count %d, error %v", published, err)
	}
}

func TestHostStateCASRespectsCancellationWhileWaitingForStore(t *testing.T) {
	manager := NewManager(t.TempDir())
	scope := sdk.Scope{Plugin: "fixture"}
	before, err := manager.Store().StateCAS(scope, "cursor", 0, json.RawMessage(`"original"`))
	if err != nil {
		t.Fatal(err)
	}
	unlock, held := filelock.Lock(filepath.Join(manager.Namespace(), "private", ".lock"), 0)
	if !held {
		t.Fatal("could not hold the private store")
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	ctx = context.WithValue(ctx, hostAuthorityKey{}, hostAuthority{manifest: sdk.Manifest{ID: scope.Plugin}})
	result := make(chan error, 1)
	go func() {
		_, err := manager.hostCall(ctx, scope, sdk.MethodStateCAS, json.RawMessage(`{"key":"cursor","version":"1","value":"replaced"}`))
		result <- err
	}()
	select {
	case err = <-result:
	case <-time.After(time.Second):
		t.Error("canceled state callback continued waiting on the private store")
		unlock()
		err = <-result
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("state callback error = %v", err)
	}
	unlock()
	after, err := manager.Store().StateGet(scope, "cursor")
	if err != nil || after.Version != before.Version || string(after.Value) != string(before.Value) {
		t.Fatalf("canceled state callback altered the value: %#v %v", after, err)
	}
}
