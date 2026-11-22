package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestStoreLargeAccountSnapshotChecksCurrentPublication(t *testing.T) {
	store := NewStore(t.TempDir())
	ctx := t.Context()
	scope := storeTestScope()
	data := accountSnapshotFixture(t, scope, 1, storeSnapshotLimit+1)
	path := writeAccountSnapshotFixture(t, store, scope.Plugin, data)
	check := func() {
		t.Helper()
		if err := store.CheckAccountGenerationContext(ctx, scope); err != nil {
			t.Fatal(err)
		}
	}
	check()
	first := loadedAccountSnapshot(t, store, scope.Plugin)
	firstFile, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if next := loadedAccountSnapshot(t, store, scope.Plugin); next != first {
		t.Fatal("unchanged large account file was decoded again")
	}
	account, err := store.GetAccountContext(ctx, scope.Plugin, scope.Provider, scope.Account)
	if err != nil {
		t.Fatal(err)
	}
	account.Auth[0] = '!'
	account, err = store.GetAccountContext(ctx, scope.Plugin, scope.Provider, scope.Account)
	if err != nil || !json.Valid(account.Auth) {
		t.Fatalf("caller mutation reached the cached account: %v", err)
	}

	writeAccountSnapshotFixture(t, store, scope.Plugin, data)
	if err := os.Chtimes(path, firstFile.ModTime(), firstFile.ModTime()); err != nil {
		t.Fatal(err)
	}
	replaced := loadedAccountSnapshot(t, store, scope.Plugin)
	replacedFile, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if replaced == first || os.SameFile(replacedFile, firstFile) ||
		!replacedFile.ModTime().Equal(firstFile.ModTime()) {
		t.Fatal("same-content replacement did not load a new publication")
	}
	rewriteStoreReadFixture(t, path, []byte(`"generation":1`), []byte(`"generation":2`))
	storeTestError(t, store.CheckAccountGenerationContext(ctx, scope), "stale_generation")
	scope.Generation = 2
	check()
	second := loadedAccountSnapshot(t, store, scope.Plugin)

	rewriteStoreReadFixture(t, path, []byte(`"generation":2`), []byte(`"generation":0`))
	storeTestError(t, store.CheckAccountGenerationContext(ctx, scope), "store_unreadable")
	rewriteStoreReadFixture(t, path, []byte(`"generation":0`), []byte(`"generation":2`))
	check()
	repaired := loadedAccountSnapshot(t, store, scope.Plugin)
	if repaired == second {
		t.Fatal("repair reused a snapshot displaced by malformed bytes")
	}

	displaced := path + ".displaced"
	if err := os.Rename(path, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	storeTestError(t, store.CheckAccountGenerationContext(ctx, scope), "store_unreadable")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	storeTestError(t, store.CheckAccountGenerationContext(ctx, scope), "stale_generation")
	if err := os.Rename(displaced, path); err != nil {
		t.Fatal(err)
	}
	check()
	if next := loadedAccountSnapshot(t, store, scope.Plugin); next == repaired {
		t.Fatal("missing publication did not evict its snapshot")
	}
}

func TestStoreLargeAccountSnapshotKeepsOneBoundedSlot(t *testing.T) {
	const maxLargeSnapshot = 128 << 10
	store := NewStore(t.TempDir())
	scope := storeTestScope()
	first := scope
	first.Plugin += "-large"
	second := scope
	second.Plugin += "-other"
	for _, selected := range []string{first.Plugin, second.Plugin, scope.Plugin} {
		selectedScope := scope
		selectedScope.Plugin = selected
		size := storeSnapshotLimit + 1
		if selected == scope.Plugin {
			size = 0
		}
		writeAccountSnapshotFixture(t, store, selected, accountSnapshotFixture(t, selectedScope, 1, size))
	}

	small := loadedAccountSnapshot(t, store, scope.Plugin)
	large := loadedAccountSnapshot(t, store, first.Plugin)
	other := loadedAccountSnapshot(t, store, second.Plugin)
	if loadedAccountSnapshot(t, store, second.Plugin) != other ||
		loadedAccountSnapshot(t, store, first.Plugin) == large ||
		loadedAccountSnapshot(t, store, scope.Plugin) != small {
		t.Fatal("large account snapshots failed to evict independently of small snapshots")
	}

	writeAccountSnapshotFixture(t, store, first.Plugin, accountSnapshotFixture(t, first, 1, maxLargeSnapshot))
	atLimit := loadedAccountSnapshot(t, store, first.Plugin)
	if loadedAccountSnapshot(t, store, first.Plugin) != atLimit {
		t.Fatal("large account snapshot at its limit was not reused")
	}
	writeAccountSnapshotFixture(t, store, first.Plugin, accountSnapshotFixture(t, first, 1, maxLargeSnapshot+1))
	overLimit := loadedAccountSnapshot(t, store, first.Plugin)
	if loadedAccountSnapshot(t, store, first.Plugin) == overLimit {
		t.Fatal("decoded account data over the large snapshot limit was retained")
	}
}

func TestStoreLargeAccountSnapshotDoesNotConfirmDurability(t *testing.T) {
	ctx := t.Context()
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	store := manager.Store()
	scope := storeTestScope()
	data := accountSnapshotFixture(t, scope, 1, storeSnapshotLimit+1)
	writeAccountSnapshotFixture(t, store, scope.Plugin, data)
	loadedAccountSnapshot(t, store, scope.Plugin)

	syncErr := errors.New("fixture directory sync failed")
	confirmations := 0
	for _, confirmErr := range []error{nil, syncErr} {
		stamp, err := store.usageStampContext(ctx, scope, func(*os.Root) error {
			confirmations++
			return confirmErr
		})
		if !errors.Is(err, confirmErr) || confirmErr != nil && stamp != (UsageStamp{}) ||
			confirmErr == nil && stamp.Scope.Generation != scope.Generation {
			t.Fatalf("usage durability returned generation %d, error %v, want %v", stamp.Scope.Generation, err, confirmErr)
		}
	}
	if confirmations != 2 || store.accountSync[store.accountSnapshotIndex(scope.Plugin)] != nil {
		t.Fatalf("large account view recorded durability instead of confirming each time: %d confirmations", confirmations)
	}
}

func loadedAccountSnapshot(t *testing.T, store *Store, plugin string) *storeAccount {
	t.Helper()
	var row *storeAccount
	err := store.withReadStoreContext(t.Context(), plugin, func(root *os.Root) error {
		records, err := store.readAccounts(root, plugin)
		if err == nil && len(records.Accounts) == 1 {
			row = &records.Accounts[0]
		}
		return err
	})
	if err != nil || row == nil {
		t.Fatalf("single account fixture could not be loaded: %v", err)
	}
	return row
}
