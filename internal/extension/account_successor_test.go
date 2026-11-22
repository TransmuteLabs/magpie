package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestAccountAuthorityReadsDoNotCreateStore(t *testing.T) {
	scope := storeTestScope()
	for _, read := range []struct {
		name, code string
		call       func(*Store) error
	}{
		{"refreshed", "not_found", func(store *Store) error {
			_, err := store.RefreshedAccountContext(t.Context(), scope)
			return err
		}},
		{"usage", "stale_generation", func(store *Store) error {
			_, err := store.UsageStamp(scope)
			return err
		}},
	} {
		t.Run(read.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "extensions")
			storeTestError(t, read.call(NewStore(root)), read.code)
			if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("lookup created the missing account store: %v", err)
			}
		})
	}
}

func TestAccountSuccessorReadsWithExistingSharedLock(t *testing.T) {
	store, root := storeTest(t)
	scope := storeTestScope()
	storeTestAccount(t, store, scope)
	lock, err := os.Open(filepath.Join(root, "private", ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if held, err := filelock.TryShared(lock); err != nil || !held {
		t.Fatalf("fixture shared lock: held %v, error %v", held, err)
	}
	defer filelock.Unlock(lock)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if current, err := NewStore(root).RefreshedAccountContext(ctx, scope); err != nil || current.Generation != scope.Generation {
		t.Fatalf("account could not be read alongside the shared lock: generation %d, error %v", current.Generation, err)
	}
}

func TestAccountSuccessorRequiresDurableRefreshReceipt(t *testing.T) {
	store := NewStore(t.TempDir())
	scope := sdk.Scope{Plugin: "fixture", Provider: "vendor", Account: "alice", Generation: 1, Operation: "refresh-fixture", Attempt: 1}
	original := Account{Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account, Auth: json.RawMessage(`{"access":"old"}`)}
	if _, err := store.SaveAccount(original, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.claimRefresh(scope, "revision", "0"); err != nil {
		t.Fatal(err)
	}
	result := sdk.AuthResult{ID: scope.Account, Credentials: json.RawMessage(`{"access":"rotated"}`),
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if _, err := store.finishRefresh(scope, &result, "completed"); err != nil {
		t.Fatal(err)
	}
	current, err := store.RefreshedAccount(scope)
	if err != nil || current.Generation != 2 || string(current.Auth) != `{"access":"rotated"}` {
		t.Fatalf("durable refresh successor was unavailable: generation=%d %v", current.Generation, err)
	}
	unconfirmedStore := NewStore(store.root)
	for _, generation := range []uint64{1, 2} {
		selected := scope
		selected.Generation = generation
		unconfirmed := errors.New("fixture directory sync failed")
		current, err := unconfirmedStore.refreshedAccount(t.Context(), selected, func(*os.Root) error { return unconfirmed })
		if !errors.Is(err, unconfirmed) || current.Generation != 0 || len(current.Auth) != 0 {
			t.Fatalf("unconfirmed refresh publication was adopted from generation %d: %+v %v", generation, current, err)
		}
	}
	original.Auth = json.RawMessage(`{"access":"manual"}`)
	if _, err := store.SaveAccount(original, 2); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []uint64{1, 2} {
		stale := scope
		stale.Generation = generation
		if _, err := store.RefreshedAccount(stale); err == nil {
			t.Fatalf("manual replacement was adopted as an owned refresh from generation %d", generation)
		}
	}
}

func TestAccountAuthorityConfirmsEachPublicationBeforeReuse(t *testing.T) {
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	store, root := manager.Store(), manager.Namespace()
	scope := storeTestScope()
	storeTestAccount(t, store, scope)
	if _, err := store.claimRefresh(scope, "revision", "0"); err != nil {
		t.Fatal(err)
	}
	result := sdk.AuthResult{ID: scope.Account, Credentials: json.RawMessage(`{"token":"rotated-fixture"}`)}
	if _, err := store.finishRefresh(scope, &result, "completed"); err != nil {
		t.Fatal(err)
	}
	confirmed := 0
	confirmation := func(directory *os.Root) error {
		confirmed++
		return storeSync(directory)
	}
	current := scope
	current.Generation++
	ctx := t.Context()
	for _, selected := range []sdk.Scope{scope, current, scope} {
		account, err := store.refreshedAccount(ctx, selected, confirmation)
		if err != nil || account.Generation != current.Generation || !json.Valid(account.Auth) {
			t.Fatalf("confirmed account: generation %d, error %v", account.Generation, err)
		}
		account.Auth[0] = '!'
		stamp, err := store.usageStampContext(ctx, current, confirmation)
		if err != nil || stamp.Dirty || stamp.Revision != 0 {
			t.Fatalf("confirmed usage: %+v, %v", stamp, err)
		}
	}
	warmConfirmations := 1
	if runtime.GOOS == "windows" {
		warmConfirmations = 6
	}
	if confirmed != warmConfirmations {
		t.Fatalf("unchanged publication confirmed %d times, want %d", confirmed, warmConfirmations)
	}

	directory := filepath.Join(root, "private", scope.Plugin)
	path := filepath.Join(directory, "accounts.json")
	directoryBefore, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(directory, "accounts.replacement")
	if err := os.WriteFile(next, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(next, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(directory, directoryBefore.ModTime(), directoryBefore.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("fixture did not replace the file while preserving metadata: %v", err)
	}
	syncError := errors.New("fixture directory sync failed")
	fail := func(*os.Root) error { return syncError }
	if account, err := store.refreshedAccount(ctx, scope, fail); !errors.Is(err, syncError) || account.Generation != 0 || len(account.Auth) != 0 {
		t.Fatalf("unconfirmed replacement allowed account use: generation %d, error %v", account.Generation, err)
	}
	if stamp, err := store.usageStampContext(ctx, current, fail); !errors.Is(err, syncError) || stamp != (UsageStamp{}) {
		t.Fatalf("unconfirmed replacement allowed usage: %+v, %v", stamp, err)
	}
	if stamp, err := store.usageStampContext(ctx, current, confirmation); err != nil || stamp.Dirty || confirmed != warmConfirmations+1 {
		t.Fatalf("replacement did not reopen after confirmation: %+v, confirmations %d, error %v", stamp, confirmed, err)
	}

	if _, err := NewStore(root).InvalidateUsage(current); err != nil {
		t.Fatal(err)
	}
	if stamp, err := store.usageStampContext(ctx, current, fail); err != nil || !stamp.Dirty || stamp.Revision != 1 {
		t.Fatalf("foreign invalidation was not visible: %+v, %v", stamp, err)
	}
	if _, err := NewStore(root).AcknowledgeUsage(current, 1); err != nil {
		t.Fatal(err)
	}
	if stamp, err := store.usageStampContext(ctx, current, fail); !errors.Is(err, syncError) || stamp != (UsageStamp{}) {
		t.Fatalf("foreign acknowledgment acquired unconfirmed authority: %+v, %v", stamp, err)
	}
	if stamp, err := store.usageStampContext(ctx, current, confirmation); err != nil || stamp.Dirty || stamp.Revision != 1 || confirmed != warmConfirmations+2 {
		t.Fatalf("foreign acknowledgment failed confirmation: %+v, confirmations %d, error %v", stamp, confirmed, err)
	}
}

func TestAccountAuthorityPinsOnlyCurrentConfirmedPublication(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory durability does not use a descriptor")
	}
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	store, root := manager.Store(), manager.Namespace()
	scope := storeTestScope()
	storeTestAccount(t, store, scope)
	if _, err := store.UsageStampContext(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	slot := store.accountSnapshotIndex(scope.Plugin)
	previous := store.accountSync[slot]
	if previous == nil {
		t.Fatal("confirmed publication has no open identity")
	}
	path := filepath.Join(root, "private", scope.Plugin, "accounts.json")
	old, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := NewStore(root)
	if _, err := writer.InvalidateUsage(scope); err != nil {
		t.Fatal(err)
	}
	displaced, err := previous.file.Stat()
	if err != nil || !os.SameFile(displaced, old) {
		t.Fatalf("displaced publication identity was released early: %v", err)
	}
	if _, err := writer.AcknowledgeUsage(scope, 1); err != nil {
		t.Fatal(err)
	}
	if stamp, err := store.UsageStampContext(t.Context(), scope); err != nil || stamp.Dirty || stamp.Revision != 1 {
		t.Fatalf("replacement usage: %+v, error %v", stamp, err)
	}
	if _, err := previous.file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("displaced account file was retained: %v", err)
	}
	if _, err := previous.directory.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("displaced account directory was retained: %v", err)
	}
}

func TestManagerCloseReleasesAccountConfirmation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory durability does not use a descriptor")
	}
	for _, started := range []bool{false, true} {
		name := "metadata_only"
		if started {
			name = "started"
		}
		t.Run(name, func(t *testing.T) {
			manager := NewManager(t.TempDir())
			store := manager.Store()
			scope := storeTestScope()
			storeTestAccount(t, store, scope)
			if started {
				manager.startLifecycleLocked()
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if _, err := store.UsageStampContext(ctx, scope); err != nil {
				t.Fatal(err)
			}
			pin := store.accountSync[store.accountSnapshotIndex(scope.Plugin)]
			if pin == nil {
				t.Fatal("account confirmation did not pin the file and directory")
			}
			t.Cleanup(pin.close)
			for range 2 {
				if err := manager.Close(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pin.file.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Errorf("closed manager retained the account file: %v", err)
			}
			if _, err := pin.directory.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Errorf("closed manager retained the account directory: %v", err)
			}
			syncError := errors.New("fixture directory sync failed")
			stamp, err := store.usageStampContext(ctx, scope, func(*os.Root) error { return syncError })
			if !errors.Is(err, syncError) || stamp != (UsageStamp{}) {
				t.Errorf("closed manager reused a cached confirmation: revision %d, error %v", stamp.Revision, err)
			}
		})
	}
}

func TestAccountConfirmationInFlightDuringManagerClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory durability does not use a descriptor")
	}
	manager := NewManager(t.TempDir())
	store := manager.Store()
	scope := storeTestScope()
	storeTestAccount(t, store, scope)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var confirmErr error
	go func() {
		defer close(done)
		_, confirmErr = store.usageStampContext(ctx, scope, func(root *os.Root) error {
			close(entered)
			select {
			case <-release:
				return storeSync(root)
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	defer func() {
		cancel()
		<-done
	}()
	select {
	case <-entered:
	case <-done:
		t.Fatalf("fixture ended before confirmation: %v", confirmErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if confirmErr != nil {
		t.Fatalf("in-flight durable confirmation failed: %v", confirmErr)
	}
	if pin := store.accountSync[store.accountSnapshotIndex(scope.Plugin)]; pin != nil {
		pin.close()
		t.Fatal("in-flight confirmation published an identity after Manager.Close")
	}
	if _, err := store.UsageStampContext(ctx, scope); err != nil {
		t.Fatalf("uncached confirmation after Manager.Close: %v", err)
	}
	if pin := store.accountSync[store.accountSnapshotIndex(scope.Plugin)]; pin != nil {
		pin.close()
		t.Fatal("new confirmation published an identity after Manager.Close")
	}
}

func TestStandaloneStoreDoesNotPinAccountConfirmation(t *testing.T) {
	store := NewStore(t.TempDir())
	scope := storeTestScope()
	storeTestAccount(t, store, scope)
	if _, err := store.UsageStampContext(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	if pin := store.accountSync[store.accountSnapshotIndex(scope.Plugin)]; pin != nil {
		pin.close()
		t.Fatal("store without a lifecycle owner retained an account confirmation")
	}
}
