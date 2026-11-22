package extension

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestUsageSnapshotDoesNotBlockPeerEnablement(t *testing.T) {
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { managerReleaseClose(t, manager) })
	selected := catalogTestInstall(t, manager.catalog, catalogTestPackage(t, "usage-module", "1.0.0"))
	other := catalogTestInstall(t, manager.catalog, catalogTestPackage(t, "other-module", "1.0.0"))
	account, err := manager.store.SaveAccount(Account{
		Plugin: selected.Manifest.ID, Provider: "vendor", ID: "alice", Auth: json.RawMessage(`{"token":"fixture"}`),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: account.Plugin, Provider: account.Provider, Account: account.ID, Generation: account.Generation}
	before, err := manager.UsageSnapshot(t.Context(), []sdk.Scope{scope})
	if err != nil || before.Accounts[scope].Scope != scope {
		t.Fatalf("initial snapshot lacks the selected account: %+v, %v", before, err)
	}
	peer := NewCatalog(manager.Namespace())
	release := sync.OnceFunc(holdPrivateStoreTestLock(t, manager.store))
	defer release()

	synctest.Test(t, func(t *testing.T) {
		type result struct {
			snapshot UsageSnapshot
			err      error
		}
		done := make(chan result, 1)
		go func() {
			snapshot, err := manager.UsageSnapshot(t.Context(), []sdk.Scope{scope})
			done <- result{snapshot, err}
		}()
		synctest.Wait()
		select {
		case read := <-done:
			t.Fatalf("snapshot returned before Store release: %+v", read)
		default:
		}

		deadline, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		disableErr := peer.SetEnabled(deadline, other.Manifest.ID, false)
		cycleErr := disableErr
		if cycleErr == nil {
			cycleErr = peer.SetEnabled(deadline, scope.Plugin, false)
		}
		if cycleErr == nil {
			cycleErr = peer.SetEnabled(deadline, scope.Plugin, true)
		}
		release()
		read := <-done
		if disableErr != nil {
			t.Fatalf("peer disable could not commit while the private Store was held: %v", disableErr)
		}
		if cycleErr != nil {
			t.Fatalf("peer cycle could not commit while the private Store was held: %v", cycleErr)
		}
		module := read.snapshot.Modules[scope.Plugin]
		if read.err != nil || read.snapshot.Accounts[scope] != before.Accounts[scope] ||
			module.PackageRevision != selected.Revision || module.EnableGeneration == "" ||
			module.EnableGeneration == before.Modules[scope.Plugin].EnableGeneration {
			t.Fatalf("snapshot did not restart with the new enable generation: %+v, %v", read.snapshot, read.err)
		}
	})
}
