package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

func TestStoreLargeAccountSnapshotConcurrentReaders(t *testing.T) {
	for index := range 8 {
		t.Run(fmt.Sprintf("store-%d", index), func(t *testing.T) {
			t.Parallel()
			store := NewStore(t.TempDir())
			scope := storeTestScope()
			scope.Generation += uint64(index)
			size := storeSnapshotLimit + 1 + index*1024
			data := accountSnapshotFixture(t, scope, 1, size)
			want, err := storeDecodeAccounts(data, scope.Plugin)
			if err != nil {
				t.Fatal(err)
			}
			writeAccountSnapshotFixture(t, store, scope.Plugin, data)
			for range 16 {
				account, err := store.GetAccountContext(t.Context(), scope.Plugin, scope.Provider, scope.Account)
				if err != nil || account.Generation != scope.Generation || !bytes.Equal(account.Auth, want.Accounts[0].Auth) {
					t.Fatalf("incorrect account across concurrent reads: generation=%d, auth bytes=%d, error=%v",
						account.Generation, len(account.Auth), err)
				}
			}
		})
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

func TestStoreLargeAccountSnapshotReleasesDisplacedConfirmation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory durability does not use descriptors")
	}
	ctx := t.Context()
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	store := manager.Store()
	writer := NewStore(manager.Namespace())
	scope := storeTestScope()
	scope.Operation, scope.Attempt = "", 0
	colliding := scope
	slot := store.accountSnapshotIndex(scope.Plugin)
	for index := 0; ; index++ {
		colliding.Plugin = fmt.Sprintf("test-confirmation-%d", index)
		if store.accountSnapshotIndex(colliding.Plugin) == slot {
			break
		}
	}
	storeTestAccount(t, writer, scope)
	storeTestAccount(t, writer, colliding)
	if _, err := store.UsageStampContext(ctx, scope); err != nil {
		t.Fatal(err)
	}
	displaced := store.accountSync[slot]
	if displaced == nil {
		t.Fatal("small publication did not pin its file and directory")
	}

	requireOldPin := func() {
		t.Helper()
		if store.accountSync[slot] != displaced {
			t.Fatal("unsuccessful read removed the confirmed pin")
		}
		for _, file := range []*os.File{displaced.file, displaced.directory} {
			if _, err := file.Stat(); err != nil {
				t.Fatalf("unsuccessful read closed a descriptor: %v", err)
			}
		}
	}
	writeAccountSnapshotFixture(t, writer, scope.Plugin, []byte(`{}`))
	_, err := store.UsageStampContext(ctx, scope)
	storeTestError(t, err, "store_unreadable")
	requireOldPin()

	writeAccountSnapshotFixture(t, writer, scope.Plugin, accountSnapshotFixture(t, scope, 1, storeSnapshotLimit+1))
	if _, err := writer.InvalidateUsage(scope); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.AcknowledgeUsage(scope, 1); err != nil {
		t.Fatal(err)
	}
	syncErr := errors.New("fixture directory sync failed")
	if stamp, err := store.usageStampContext(ctx, scope, func(*os.Root) error { return syncErr }); !errors.Is(err, syncErr) || stamp != (UsageStamp{}) {
		t.Fatalf("unconfirmed usage returned %+v, %v", stamp, err)
	}
	requireOldPin()
	canceled, cancel := context.WithCancel(ctx)
	defer cancel()
	if stamp, err := store.usageStampContext(canceled, scope, func(root *os.Root) error {
		cancel()
		return storeSync(root)
	}); !errors.Is(err, context.Canceled) || stamp != (UsageStamp{}) {
		t.Fatalf("canceled usage returned %+v, %v", stamp, err)
	}
	requireOldPin()

	want := UsageStamp{Scope: scope, Revision: 1}
	if stamp, err := store.UsageStampContext(ctx, scope); err != nil || stamp != want {
		t.Fatalf("large publication returned %+v, %v, want %+v", stamp, err, want)
	}
	if store.accountSync[slot] != nil {
		t.Error("large publication retained a small confirmation pin")
	}
	for _, file := range []*os.File{displaced.file, displaced.directory} {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Errorf("large publication retained a displaced descriptor: %v", err)
		}
	}

	if _, err := store.UsageStampContext(ctx, colliding); err != nil {
		t.Fatal(err)
	}
	other := store.accountSync[slot]
	if other == nil || other.namespace != colliding.Plugin {
		t.Fatal("colliding namespace did not pin its own publication")
	}
	confirmations := 0
	for range 2 {
		if stamp, err := store.usageStampContext(ctx, scope, func(root *os.Root) error {
			confirmations++
			return storeSync(root)
		}); err != nil || stamp != want {
			t.Fatalf("repeated large publication returned %+v, %v", stamp, err)
		}
	}
	if confirmations != 2 || store.accountSync[slot] != other {
		t.Fatalf("got %d syncs or a displaced confirmation from another namespace", confirmations)
	}
}

func TestStoreLargeAccountSnapshotKeepsReplacementRootConfirmation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory durability does not use descriptors")
	}
	ctx := t.Context()
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	store := manager.Store()
	writer := NewStore(manager.Namespace())
	scope := storeTestScope()
	path := writeAccountSnapshotFixture(t, writer, scope.Plugin, accountSnapshotFixture(t, scope, 1, storeSnapshotLimit+1))
	directory := filepath.Dir(path)
	displaced, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer displaced.Close()
	snapshot, err := storeLoadAccountSnapshot(displaced, scope.Plugin, store)
	if err != nil || snapshot == nil {
		t.Fatalf("load prior rooted publication: %v", err)
	}
	if err := os.Rename(directory, directory+".displaced"); err != nil {
		t.Fatal(err)
	}
	storeTestAccount(t, writer, scope)
	if _, err := store.UsageStampContext(ctx, scope); err != nil {
		t.Fatal(err)
	}
	slot := store.accountSnapshotIndex(scope.Plugin)
	current := store.accountSync[slot]
	if current == nil {
		t.Fatal("replacement root did not pin its small publication")
	}
	if err := store.confirmAccountSnapshot(ctx, displaced, snapshot, storeSync); err != nil {
		t.Fatalf("confirm a previously rooted publication: %v", err)
	}
	if store.accountSync[slot] != current {
		t.Fatal("previously rooted read removed the replacement root's pin")
	}
	if _, err := store.usageStampContext(ctx, scope, func(*os.Root) error {
		return errors.New("replacement root was already confirmed")
	}); err != nil {
		t.Fatalf("replacement root lost its durability proof: %v", err)
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
