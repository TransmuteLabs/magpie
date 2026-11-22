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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestStoreReadDoesNotCreateMissingPaths(t *testing.T) {
	for _, present := range []string{"none", "root", "private", "namespace", "locked-namespace"} {
		t.Run(present, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "extensions")
			private := filepath.Join(root, "private")
			scope := storeTestScope()
			namespace := filepath.Join(private, scope.Plugin)
			if present == "locked-namespace" {
				storeTestAccount(t, NewStore(root), scope)
			} else if directory := map[string]string{"root": root, "private": private, "namespace": namespace}[present]; directory != "" {
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			store := NewStore(root)
			ctx := t.Context()
			value, err := store.StateGetContext(ctx, sdk.Scope{Plugin: scope.Plugin}, "missing")
			if err != nil || value.Version != 0 || len(value.Value) != 0 {
				t.Fatalf("missing module state: version %d, size %d, error %v", value.Version, len(value.Value), err)
			}
			value, err = store.StateGetContext(ctx, scope, "missing")
			if present == "locked-namespace" {
				if err != nil || value.Version != 0 || len(value.Value) != 0 {
					t.Fatalf("missing account state: version %d, size %d, error %v", value.Version, len(value.Value), err)
				}
			} else {
				storeTestError(t, err, "stale_generation")
			}
			rows, err := store.AccountsContext(ctx, scope.Plugin, "missing")
			if err != nil || rows == nil || len(rows) != 0 {
				t.Fatalf("missing account list: length %d, error %v", len(rows), err)
			}
			generations, err := store.AccountGenerationsContext(ctx, scope.Plugin, "missing")
			if err != nil || generations == nil || len(generations) != 0 {
				t.Fatalf("missing account generations: length %d, error %v", len(generations), err)
			}
			metadata, err := store.AccountsMetadataContext(ctx, "missing-plugin", nil, true)
			if err != nil || len(metadata) != 0 {
				t.Fatalf("missing module metadata: length %d, error %v", len(metadata), err)
			}
			_, err = store.GetAccountContext(ctx, scope.Plugin, scope.Provider, "missing")
			storeTestError(t, err, "not_found")
			owners, err := store.SubscriptionOwners(ctx)
			if err != nil || len(owners) != 0 {
				t.Fatalf("missing owners: length %d, error %v", len(owners), err)
			}
			missing := []string{filepath.Join(namespace, "state"), filepath.Join(private, savedSignInsDir), filepath.Join(private, "missing-plugin")}
			switch present {
			case "none":
				missing = append(missing, root)
			case "root":
				missing = append(missing, private)
			case "private":
				missing = append(missing, namespace)
			}
			if present != "locked-namespace" {
				missing = append(missing, filepath.Join(private, ".lock"))
			}
			for _, path := range missing {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("read created or could not inspect %s: %v", path, err)
				}
			}
		})
	}
}

func storeReadFixture(test testing.TB, count int) (*Store, sdk.Scope) {
	test.Helper()
	store := NewStore(filepath.Join(test.TempDir(), "extensions"))
	scope := storeTestScope()
	if err := store.withPrivateStore(test.Context(), func(private *os.Root) error {
		module, err := storeDirectory(private, scope.Plugin)
		if err != nil {
			return err
		}
		defer module.Close()
		saved, err := storeDirectory(private, savedSignInsDir)
		if err != nil {
			return err
		}
		defer saved.Close()
		accounts := storeAccounts{Schema: 4, AuthFlows: []windowAuthRecord{}}
		owner := subscriptionRecord{
			SubscriptionOwner: SubscriptionOwner{Module: scope.Plugin, Provider: scope.Provider, PublicID: scope.Provider, Revision: "revision", Active: true},
			GrantRevision:     strings.Repeat("b", 64),
			Digests:           make(map[string]string, count),
		}
		for index := range count {
			id := fmt.Sprintf("account-%02d", index)
			if index == 0 {
				id = scope.Account
			}
			accounts.Accounts = append(accounts.Accounts, storeAccount{
				Account: Account{Plugin: scope.Plugin, Provider: scope.Provider, ID: id, Generation: 1,
					Auth: json.RawMessage(`{"token":"` + strings.Repeat("a", 768) + `"}`)},
				Usage: &usageState{},
			})
			owner.Accounts = append(owner.Accounts, SubscriptionAccount{Key: id, ID: id, User: id, LoginID: id})
			owner.Digests[id] = strings.Repeat("a", 64)
			owner.Tickets = append(owner.Tickets, fmt.Sprintf("ticket-%02d", index))
		}
		if err := storeWrite(module, "accounts.json", accounts, storeFileLimit); err != nil {
			return err
		}
		return storeWrite(saved, subscriptionOwnersFile, []subscriptionRecord{owner}, storeFileLimit)
	}); err != nil {
		test.Fatal(err)
	}
	if _, err := store.StateCAS(scope, "key", 0, json.RawMessage(`{"count":1}`)); err != nil {
		test.Fatal(err)
	}
	return store, scope
}

func storeReadOperations(store *Store, scope sdk.Scope) []struct {
	name string
	read func(context.Context) error
} {
	return []struct {
		name string
		read func(context.Context) error
	}{
		{"state", func(ctx context.Context) error {
			row, err := store.StateGetContext(ctx, scope, "key")
			if err == nil && row.Version != 1 {
				return fmt.Errorf("state version = %d", row.Version)
			}
			return err
		}},
		{"accounts", func(ctx context.Context) error {
			rows, err := store.AccountsContext(ctx, scope.Plugin, scope.Provider)
			if err == nil && len(rows) != 1 {
				return fmt.Errorf("account count = %d", len(rows))
			}
			return err
		}},
		{"account", func(ctx context.Context) error {
			row, err := store.GetAccountContext(ctx, scope.Plugin, scope.Provider, scope.Account)
			if err == nil && row.Generation != scope.Generation {
				return fmt.Errorf("account generation = %d", row.Generation)
			}
			return err
		}},
		{"refreshed-account", func(ctx context.Context) error {
			row, err := store.RefreshedAccountContext(ctx, scope)
			if err == nil && row.Generation != scope.Generation {
				return fmt.Errorf("refreshed account generation = %d", row.Generation)
			}
			return err
		}},
		{"usage-stamp", func(ctx context.Context) error {
			stamp, err := store.UsageStampContext(ctx, scope)
			if err == nil && (stamp.Scope.Generation != scope.Generation || stamp.Dirty) {
				return fmt.Errorf("usage stamp = %v", stamp)
			}
			return err
		}},
		{"metadata", func(ctx context.Context) error {
			rows, err := store.AccountsMetadataContext(ctx, scope.Plugin, nil, true)
			if err == nil && len(rows[scope.Provider]) != 1 {
				return fmt.Errorf("metadata count = %d", len(rows[scope.Provider]))
			}
			return err
		}},
		{"generations", func(ctx context.Context) error {
			rows, err := store.AccountGenerationsContext(ctx, scope.Plugin, scope.Provider)
			if err == nil && (len(rows) != 1 || rows[0].Generation != scope.Generation) {
				return fmt.Errorf("generations = %v", rows)
			}
			return err
		}},
		{"owners", func(ctx context.Context) error {
			rows, err := store.SubscriptionOwners(ctx)
			if err == nil && (len(rows) != 1 || !rows[0].Active) {
				return fmt.Errorf("active owner unavailable")
			}
			return err
		}},
	}
}

func TestStoreReadSharesExistingLock(t *testing.T) {
	store, scope := storeReadFixture(t, 1)
	private := filepath.Join(store.root, "private")
	lockPath := filepath.Join(private, ".lock")
	lock, err := os.Open(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if held, err := filelock.TryShared(lock); err != nil || !held {
		t.Fatalf("fixture shared lock: held %v, error %v", held, err)
	}
	defer filelock.Unlock(lock)
	if runtime.GOOS != "windows" {
		if err = os.Chmod(filepath.Join(private, scope.Plugin, "state"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err = os.Chmod(private, 0o750); err != nil {
			t.Fatal(err)
		}
		if err = lock.Chmod(0o640); err != nil {
			t.Fatal(err)
		}
	}
	for _, operation := range storeReadOperations(NewStore(store.root), scope) {
		t.Run(operation.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := operation.read(ctx); err != nil {
				t.Fatalf("read beside held shared lock: %v", err)
			}
		})
	}
	if runtime.GOOS != "windows" {
		for path, mode := range map[string]os.FileMode{private: 0o750, lockPath: 0o640, filepath.Join(private, scope.Plugin, "state"): 0o750} {
			actual, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if actual.Mode().Perm() != mode {
				t.Errorf("read changed permissions of %s from %o to %o", path, mode, actual.Mode().Perm())
			}
		}
	}
}

func TestStoreReadCanceledBehindWriterPreservesFiles(t *testing.T) {
	store, scope := storeReadFixture(t, 1)
	private := filepath.Join(store.root, "private")
	paths := []string{
		filepath.Join(private, scope.Plugin, "accounts.json"),
		filepath.Join(private, scope.Plugin, "state", storeStateName(scope, "key")),
		filepath.Join(private, savedSignInsDir, subscriptionOwnersFile),
	}
	before := make([][]byte, len(paths))
	for index, path := range paths {
		var err error
		before[index], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	lock, err := os.OpenFile(filepath.Join(private, ".lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if held, err := filelock.TryLock(lock); err != nil || !held {
		t.Fatalf("fixture writer lock: held %v, error %v", held, err)
	}
	defer filelock.Unlock(lock)
	for _, operation := range storeReadOperations(NewStore(store.root), scope) {
		t.Run(operation.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
			defer cancel()
			if err := operation.read(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("read while writer held returned %v", err)
			}
		})
	}
	for index, path := range paths {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before[index], after) {
			t.Fatalf("canceled reader changed file %s: %v", filepath.Base(path), err)
		}
	}
}

func TestStoreReadExistingDataWithoutCreatingLock(t *testing.T) {
	store, scope := storeReadFixture(t, 1)
	lock := filepath.Join(store.root, "private", ".lock")
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	reader := NewStore(store.root)
	for _, operation := range storeReadOperations(reader, scope) {
		if err := operation.read(t.Context()); err != nil {
			t.Errorf("read %s after a restore without lock files: %v", operation.name, err)
		}
	}
	if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reader created the lock for a restored store")
	}
}

func TestStoreReadRechecksFirstWriterLock(t *testing.T) {
	store, scope := storeReadFixture(t, 1)
	if err := os.Remove(filepath.Join(store.root, "private", ".lock")); err != nil {
		t.Fatal(err)
	}
	var versions []uint64
	ctx := t.Context()
	err := store.withReadStoreContext(ctx, scope.Plugin, func(root *os.Root) error {
		value, err := storeReadState(root, scope, "key")
		if err != nil {
			return err
		}
		versions = append(versions, value.Version)
		if len(versions) == 1 {
			_, err := NewStore(store.root).StateCASContext(ctx, scope, "key", 1, json.RawMessage(`{"count":2}`))
			return err
		}
		return nil
	})
	if err != nil || len(versions) != 2 || versions[0] != 1 || versions[1] != 2 {
		t.Fatalf("read versions after first writer: %v, %v", versions, err)
	}
}

func TestStoreReadAccountMetadataKeepsProviderAndLabel(t *testing.T) {
	store, scope := storeReadFixture(t, 1)
	another := "another-provider"
	writer := NewStore(store.root)
	for _, item := range []struct{ provider, name string }{{scope.Provider, "Named first"}, {another, "Named second"}} {
		generation := uint64(0)
		if item.provider == scope.Provider {
			generation = scope.Generation
		}
		if _, err := writer.SaveAccount(Account{
			Plugin: scope.Plugin, Provider: item.provider, ID: scope.Account, Name: item.name,
			Auth: json.RawMessage(`{}`), ExpiresAt: 123,
		}, generation); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string][]AccountMetadata {
		t.Helper()
		rows, err := store.AccountsMetadataContext(t.Context(), scope.Plugin, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	before := read()
	if len(before) != 2 || len(before[scope.Provider]) != 1 || len(before[another]) != 1 ||
		before[scope.Provider][0] != (AccountMetadata{ID: scope.Account, Name: "Named first", Generation: 2, ExpiresAt: 123}) ||
		before[another][0] != (AccountMetadata{ID: scope.Account, Name: "Named second", Generation: 1, ExpiresAt: 123}) {
		t.Fatalf("metadata did not preserve account and provider labels: %v", before)
	}
	schedule, err := store.AccountsMetadataContext(t.Context(), scope.Plugin, []string{another}, false)
	if err != nil || len(schedule) != 1 || !slices.Equal(schedule[another], []AccountMetadata{
		{ID: scope.Account, Generation: 1, ExpiresAt: 123},
	}) {
		t.Fatalf("renewal metadata included unselected accounts or labels: %v, %v", schedule, err)
	}
	before[another][0].Name = "modified view"
	if again := read(); len(again[another]) != 1 || again[another][0].Name != "Named second" {
		t.Fatal("metadata shares writable slices")
	}
	if err := writer.DeleteAccount(scope.Plugin, scope.Provider, scope.Account, 2); err != nil {
		t.Fatal(err)
	}
	if after := read(); len(after) != 1 || len(after[another]) != 1 {
		t.Fatal("metadata exposes tombstoned account")
	}
}

func TestStoreReadAccountBudgetPrecedesProjection(t *testing.T) {
	store, scope := storeReadFixture(t, 2)
	rows, err := projectProviderAccounts(t.Context(), store, scope.Plugin, scope.Provider, 1, func(_ *os.Root, record storeAccount) string {
		t.Error("account was enriched before checking the list budget:", record.ID)
		return record.ID
	})
	if rows != nil || !errors.Is(err, errAccountListBudget) {
		t.Fatalf("bounded account projection = %v, %v", rows, err)
	}
}

func TestStoreReadAccountGenerationsAreFilteredAndCurrent(t *testing.T) {
	store, scope := storeReadFixture(t, 1)
	writer := NewStore(store.root)
	account := Account{Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account,
		Name: "Generation projection label", Auth: json.RawMessage(`{}`)}
	if _, err := writer.SaveAccount(account, 1); err != nil {
		t.Fatal(err)
	}
	t.Log("generation label file", account.labelFile())
	foreign, deleted := account, account
	foreign.Provider, deleted.ID = "other-vendor", "deleted"
	for _, added := range []Account{foreign, deleted} {
		if _, err := writer.SaveAccount(added, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.DeleteAccount(deleted.Plugin, deleted.Provider, deleted.ID, 1); err != nil {
		t.Fatal(err)
	}
	read := func(generation uint64) []AccountGeneration {
		t.Helper()
		rows, err := store.AccountGenerationsContext(t.Context(), scope.Plugin, scope.Provider)
		if err != nil || len(rows) != 1 || rows[0] != (AccountGeneration{ID: scope.Account, Generation: generation}) {
			t.Fatalf("account generations = %v, %v", rows, err)
		}
		return rows
	}
	read(2)[0].Generation = 0
	read(2)
	if _, err := writer.SaveAccount(account, 2); err != nil {
		t.Fatal(err)
	}
	read(3)
	accountPath := filepath.Join(store.root, "private", scope.Plugin, "accounts.json")
	rewriteStoreReadFixture(t, accountPath, []byte(`"generation":1`), []byte(`"generation":0`))
	rows, err := store.AccountGenerationsContext(t.Context(), scope.Plugin, scope.Provider)
	if rows != nil {
		t.Fatalf("projection exposed records from an invalid snapshot: %v", rows)
	}
	storeTestError(t, err, "store_unreadable")
}

func TestStoreReadViewsAreIndependentAndCurrent(t *testing.T) {
	store, scope := storeReadFixture(t, 1)
	ctx := t.Context()
	list, err := store.AccountsContext(ctx, scope.Plugin, scope.Provider)
	if err != nil || len(list) != 1 {
		t.Fatalf("list accounts: length %d, error %v", len(list), err)
	}
	account, err := store.GetAccountContext(ctx, scope.Plugin, scope.Provider, scope.Account)
	if err != nil {
		t.Fatal(err)
	}
	list[0].Auth[0] = '!'
	if !json.Valid(account.Auth) {
		t.Fatal("mutation of listed auth reached a separately returned account")
	}
	account.Auth[0] = '!'
	again, err := store.GetAccountContext(ctx, scope.Plugin, scope.Provider, scope.Account)
	if err != nil || !json.Valid(again.Auth) {
		t.Fatalf("mutation of returned auth reached later reads: %v", err)
	}
	owners, err := store.SubscriptionOwners(ctx)
	if err != nil || len(owners) != 1 || len(owners[0].Accounts) != 1 {
		t.Fatalf("read owner: %v", err)
	}
	owners[0].Accounts[0].User = "modified view"
	owners, err = store.SubscriptionOwners(ctx)
	if err != nil || len(owners) != 1 || len(owners[0].Accounts) != 1 || owners[0].Accounts[0].User != scope.Account {
		t.Fatalf("mutation of returned account ownership reached later reads: %v", err)
	}
	accountPath := filepath.Join(store.root, "private", scope.Plugin, "accounts.json")
	rewriteStoreReadFixture(t, accountPath, []byte(`"generation":1`), []byte(`"generation":2`))
	again, err = store.GetAccountContext(ctx, scope.Plugin, scope.Provider, scope.Account)
	if err != nil || again.Generation != 2 {
		t.Fatalf("generation after same-metadata rewrite: %d, %v", again.Generation, err)
	}
	_, err = store.StateGetContext(ctx, scope, "key")
	storeTestError(t, err, "stale_generation")
	ownerPath := filepath.Join(store.root, "private", savedSignInsDir, subscriptionOwnersFile)
	rewriteStoreReadFixture(t, ownerPath, []byte(`"revision":"revision"`), []byte(`"revision":"modified"`))
	owners, err = store.SubscriptionOwners(ctx)
	if err != nil || len(owners) != 1 || owners[0].Revision != "modified" {
		t.Fatalf("owner revision after same-metadata rewrite: %v", err)
	}
	rewriteStoreReadFixture(t, ownerPath, []byte(`"active":true`), []byte(`"active":null`))
	_, err = store.SubscriptionOwners(ctx)
	storeTestError(t, err, "store_unreadable")
	rewriteStoreReadFixture(t, ownerPath, []byte(`"active":null`), []byte(`"active":"xx"`))
	_, err = store.SubscriptionOwners(ctx)
	storeTestError(t, err, "store_unreadable")
	rewriteStoreReadFixture(t, ownerPath, []byte(`"active":"xx"`), []byte(`"active":true`))
	owners, err = store.SubscriptionOwners(ctx)
	if err != nil || len(owners) != 1 || !owners[0].Active {
		t.Fatalf("owner after same-metadata repair: %v, %v", owners, err)
	}
}

func TestSubscriptionOwnerAuthorityFlags(t *testing.T) {
	const otherFields = `"module":"test-module","provider":"test-provider","publicID":"test-provider",` +
		`"revision":"revision","accounts":[{"key":"saved","id":"account","user":"user","loginID":"login"}],` +
		`"grantRevision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
		`"digests":{"saved":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},` +
		`"tickets":["ticket"],"generations":[1]`
	for _, test := range []struct {
		name, flags                          string
		active, pending, dispatched, invalid bool
	}{
		{name: "legacy active", flags: `,"active":true`, active: true},
		{name: "legacy rolled back", flags: `,"active":false`},
		{name: "pending", flags: `,"active":false,"pending":true`, pending: true},
		{name: "explicit false", flags: `,"active":false,"pending":false,"authDispatched":false`},
		{name: "dispatched", flags: `,"active":true,"authDispatched":true`, active: true, dispatched: true},
		{name: "missing active", invalid: true},
		{name: "null active", flags: `,"active":null`, invalid: true},
		{name: "null pending", flags: `,"active":false,"pending":null`, invalid: true},
		{name: "null dispatch fence", flags: `,"active":true,"authDispatched":null`, invalid: true},
		{name: "repeated active", flags: `,"active":true,"Active":false`, invalid: true},
		{name: "repeated pending", flags: `,"active":false,"pending":true,"pending":false`, invalid: true},
		{name: "repeated dispatch fence", flags: `,"active":true,"authDispatched":true,"authDispatched":false`, invalid: true},
		{name: "simultaneously active and pending", flags: `,"active":true,"pending":true`, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows, err := decodeSubscriptionRecords([]byte("[{" + otherFields + test.flags + "}]"))
			if test.invalid {
				storeTestError(t, err, "store_unreadable")
				if rows != nil {
					t.Fatalf("damaged ownership returned partial rows: %+v", rows)
				}
				return
			}
			if err != nil || len(rows) != 1 {
				t.Fatalf("decode owner: %v, %v", rows, err)
			}
			row := rows[0]
			if row.Active != test.active || row.Pending != test.pending || row.AuthDispatched != test.dispatched {
				t.Fatalf("authority flags = %t, %t, %t", row.Active, row.Pending, row.AuthDispatched)
			}
			if test.dispatched && !errors.Is(row.checkRollback(nil), errRollbackUnverified) {
				t.Fatal("a dispatched auth operation must keep rollback blocked")
			}
		})
	}
}

func TestStoreReadOwnersRejectsSymlinkAncestor(t *testing.T) {
	store, _ := storeReadFixture(t, 1)
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(filepath.Dir(store.root), linked); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	reader := NewStore(filepath.Join(linked, filepath.Base(store.root)))
	_, err := reader.SubscriptionOwners(t.Context())
	storeTestError(t, err, "store_unreadable")
}

func rewriteStoreReadFixture(t *testing.T, path string, old, new []byte) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != len(new) || !bytes.Contains(data, old) {
		t.Fatal("replacement must find the old value and preserve size")
	}
	if err := os.WriteFile(path, bytes.Replace(data, old, new, 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("fixture rewrite changed the inode, size or modified time")
	}
}

func BenchmarkStoreRead(b *testing.B) {
	store, scope := storeReadFixture(b, 24)
	b.Run("AccountGenerationState", func(bench *testing.B) {
		bench.ReportAllocs()
		for bench.Loop() {
			row, err := store.StateGetContext(bench.Context(), scope, "key")
			if err != nil || row.Version != 1 {
				bench.Fatalf("read account state: version %d, error %v", row.Version, err)
			}
		}
	})
	b.Run("Accounts", func(bench *testing.B) {
		bench.ReportAllocs()
		for bench.Loop() {
			rows, err := store.AccountsContext(bench.Context(), scope.Plugin, scope.Provider)
			if err != nil || len(rows) != 24 {
				bench.Fatalf("read accounts: length %d, error %v", len(rows), err)
			}
		}
	})
	b.Run("AccountMetadata", func(bench *testing.B) {
		bench.ReportAllocs()
		for bench.Loop() {
			rows, err := store.AccountsMetadataContext(bench.Context(), scope.Plugin, []string{scope.Provider}, true)
			if err != nil || len(rows[scope.Provider]) != 24 {
				bench.Fatalf("read metadata: length %d, error %v", len(rows[scope.Provider]), err)
			}
		}
	})
	b.Run("SubscriptionOwners", func(bench *testing.B) {
		bench.ReportAllocs()
		for bench.Loop() {
			rows, err := store.SubscriptionOwners(bench.Context())
			if err != nil || len(rows) != 1 {
				bench.Fatalf("read owner: length %d, error %v", len(rows), err)
			}
		}
	})
}
