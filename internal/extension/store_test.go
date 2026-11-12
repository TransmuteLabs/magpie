package extension

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func storeTest(t *testing.T) (*Store, string) {
	t.Helper()
	home := t.TempDir()
	testenv.SetHome(t, home)
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "APPDATA", "LOCALAPPDATA"} {
		t.Setenv(key, filepath.Join(home, key))
	}
	root := filepath.Join(home, "extensions")
	return NewStore(root), root
}

func storeTestScope() sdk.Scope {
	return sdk.Scope{Plugin: "test-plugin", Provider: "vendor", Account: "account", Generation: 1, Operation: "operation", Attempt: 1}
}

func storeTestAccount(t *testing.T, store *Store, scope sdk.Scope) Account {
	t.Helper()
	account, err := store.SaveAccount(Account{
		Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account,
		Auth: json.RawMessage(`{"token":"synthetic-private-secret"}`),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func storeTestError(t *testing.T, err error, code string) *sdk.Error {
	t.Helper()
	var failure *sdk.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("error = %v; want SDK error %q", err, code)
	}
	if strings.Contains(err.Error(), "synthetic-private-secret") {
		t.Fatalf("secret in error: %v", err)
	}
	return failure
}

func TestStoreStateCASAndScopeIsolation(t *testing.T) {
	store, root := storeTest(t)
	scope := storeTestScope()
	account := storeTestAccount(t, store, scope)
	key := "../../key/with/slashes"
	missing, err := store.StateGet(scope, key)
	if err != nil || missing.Version != 0 || len(missing.Value) != 0 {
		t.Fatalf("missing state = %+v, %v", missing, err)
	}
	value := json.RawMessage(`{"count":1}`)
	first, err := store.StateCAS(scope, key, 0, value)
	if err != nil || first.Version != 1 || !bytes.Equal(first.Value, value) {
		t.Fatalf("first state = %+v, %v", first, err)
	}
	value[2] = 'X'
	same := scope
	same.Session, same.Operation, same.Attempt = "other-session", "other-operation", 2
	got, err := NewStore(root).StateGet(same, key)
	if err != nil || got.Version != 1 || string(got.Value) != `{"count":1}` {
		t.Fatalf("state across calls = %+v, %v", got, err)
	}
	got.Value[2] = 'X'
	_, err = store.StateCAS(scope, key, 0, json.RawMessage(`2`))
	storeTestError(t, err, "conflict")
	second, err := store.StateCAS(scope, key, 1, json.RawMessage(`2`))
	if err != nil || second.Version != 2 {
		t.Fatalf("second state = %+v, %v", second, err)
	}
	for _, isolated := range []sdk.Scope{
		{Plugin: scope.Plugin},
		{Plugin: "other-plugin"},
		{Plugin: scope.Plugin, Provider: "other-vendor", Account: scope.Account, Generation: 1},
		{Plugin: scope.Plugin, Provider: scope.Provider, Account: "../other-account", Generation: 1},
	} {
		if isolated.Account != "" {
			storeTestAccount(t, store, isolated)
		}
		state, err := store.StateGet(isolated, key)
		if err != nil || state.Version != 0 {
			t.Fatalf("isolated state for %+v = %+v, %v", isolated, state, err)
		}
	}
	account.Auth = json.RawMessage(`{"token":"replacement"}`)
	replaced, err := store.SaveAccount(account, account.Generation)
	if err != nil || replaced.Generation != 2 {
		t.Fatalf("replacement = %+v, %v", replaced, err)
	}
	_, err = store.StateGet(scope, key)
	storeTestError(t, err, "stale_generation")
	_, err = store.StateCAS(scope, key, 2, json.RawMessage(`3`))
	storeTestError(t, err, "stale_generation")
	scope.Generation = replaced.Generation
	got, err = store.StateGet(scope, key)
	if err != nil || got.Version != 0 {
		t.Fatalf("new generation state = %+v, %v", got, err)
	}
}

func TestStoreAccountCASRevocationAndRecreation(t *testing.T) {
	store, root := storeTest(t)
	scope := storeTestScope()
	account := storeTestAccount(t, store, scope)
	_, err := store.SaveAccount(account, 0)
	storeTestError(t, err, "conflict")
	got, err := NewStore(root).GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil || got.Generation != 1 || !bytes.Equal(got.Auth, account.Auth) {
		t.Fatalf("persisted account = %+v, %v", got, err)
	}
	storeTestError(t, store.DeleteAccount(scope.Plugin, scope.Provider, scope.Account, 0), "conflict")
	if _, err := store.StateCAS(scope, "value", 0, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Journal(scope, "dispatched"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAccount(scope.Plugin, scope.Provider, scope.Account, 1); err != nil {
		t.Fatal(err)
	}
	_, err = store.GetAccount(scope.Plugin, scope.Provider, scope.Account)
	storeTestError(t, err, "not_found")
	accounts, err := store.Accounts(scope.Plugin, scope.Provider)
	if err != nil || len(accounts) != 0 {
		t.Fatalf("revoked accounts = %+v, %v", accounts, err)
	}
	_, err = store.StateGet(scope, "value")
	storeTestError(t, err, "stale_generation")
	_, err = store.StateCAS(scope, "value", 1, json.RawMessage(`2`))
	storeTestError(t, err, "stale_generation")
	_, err = store.Journal(scope, "accepted")
	if err != nil {
		t.Fatalf("revoked dispatch could not record acceptance: %v", err)
	}
	entry, err := store.Operation(scope.Plugin, scope.Operation)
	if err != nil || entry.State != "accepted" {
		t.Fatalf("revoked operation remains inspectable = %+v, %v", entry, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "private", scope.Plugin, "accounts.json"))
	if err != nil || bytes.Contains(data, account.Auth) || bytes.Contains(data, []byte("synthetic-private-secret")) {
		t.Fatalf("tombstone contains credentials: %v", err)
	}
	_, err = store.SaveAccount(account, 1)
	storeTestError(t, err, "conflict")
	recreated, err := store.SaveAccount(account, 0)
	if err != nil || recreated.Generation != 3 {
		t.Fatalf("recreated account = %+v, %v", recreated, err)
	}
	scope.Generation = recreated.Generation
	state, err := store.StateGet(scope, "value")
	if err != nil || state.Version != 0 {
		t.Fatalf("recreated state = %+v, %v", state, err)
	}
	_, err = store.Journal(scope, "dispatched")
	storeTestError(t, err, "operation_conflict")
}

func TestStoreJournalReconcilesRevokedGeneration(t *testing.T) {
	for _, revocation := range []string{"refresh", "delete"} {
		t.Run(revocation, func(t *testing.T) {
			store, root := storeTest(t)
			scope := storeTestScope()
			scope.Session = "session"
			account := storeTestAccount(t, store, scope)
			if _, err := store.StateCAS(scope, "key", 0, json.RawMessage(`1`)); err != nil {
				t.Fatal(err)
			}
			for _, state := range []string{"dispatched", "accepted"} {
				if _, err := store.Journal(scope, state); err != nil {
					t.Fatal(err)
				}
			}
			if revocation == "refresh" {
				account.Auth = json.RawMessage(`{"token":"replacement"}`)
				if _, err := store.SaveAccount(account, account.Generation); err != nil {
					t.Fatal(err)
				}
			} else if err := store.DeleteAccount(scope.Plugin, scope.Provider, scope.Account, account.Generation); err != nil {
				t.Fatal(err)
			}
			accountPath := filepath.Join(root, "private", scope.Plugin, "accounts.json")
			before, err := os.ReadFile(accountPath)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.StateGet(scope, "key")
			storeTestError(t, err, "stale_generation")
			_, err = store.StateCAS(scope, "key", 1, json.RawMessage(`2`))
			storeTestError(t, err, "stale_generation")
			newWork := scope
			newWork.Operation = "new-operation"
			_, err = store.Journal(newWork, "dispatched")
			storeTestError(t, err, "stale_generation")
			_, err = store.Operation(scope.Plugin, newWork.Operation)
			storeTestError(t, err, "not_found")
			_, err = store.Journal(newWork, "uncertain")
			storeTestError(t, err, "operation_conflict")
			for _, changed := range []sdk.Scope{
				{Plugin: scope.Plugin, Provider: scope.Provider, Account: scope.Account, Generation: 2, Session: scope.Session, Operation: scope.Operation, Attempt: scope.Attempt},
				{Plugin: scope.Plugin, Provider: scope.Provider, Account: scope.Account, Generation: scope.Generation, Session: "other-session", Operation: scope.Operation, Attempt: scope.Attempt},
				{Plugin: scope.Plugin, Provider: scope.Provider, Account: scope.Account, Generation: scope.Generation, Session: scope.Session, Operation: scope.Operation, Attempt: scope.Attempt + 1},
			} {
				_, err := store.Journal(changed, "completed")
				if failure := storeTestError(t, err, "operation_conflict"); failure.Outcome != "accepted" {
					t.Fatalf("scope conflict lost accepted outcome: %q", failure.Outcome)
				}
			}
			_, err = store.Journal(scope, "dispatched")
			if failure := storeTestError(t, err, "operation_conflict"); failure.Outcome != "accepted" {
				t.Fatalf("duplicate lost accepted outcome: %q", failure.Outcome)
			}
			_, err = store.Journal(scope, "not-started")
			if failure := storeTestError(t, err, "operation_conflict"); failure.Outcome != "accepted" {
				t.Fatalf("downgrade lost accepted outcome: %q", failure.Outcome)
			}
			for _, transition := range []struct {
				request string
				state   string
			}{{"canceled", "uncertain"}, {"completed", "completed"}} {
				entry, err := store.Journal(scope, transition.request)
				if err != nil || entry.Scope != scope || entry.State != transition.state {
					t.Fatalf("revoked %s = %+v, %v", transition.request, entry, err)
				}
				persisted, err := NewStore(root).Operation(scope.Plugin, scope.Operation)
				if err != nil || persisted != entry {
					t.Fatalf("reconciled journal = %+v, %v", persisted, err)
				}
			}
			after, err := os.ReadFile(accountPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("journal reconciliation changed credentials: %v", err)
			}
			_, err = store.StateCAS(scope, "key", 1, json.RawMessage(`2`))
			storeTestError(t, err, "stale_generation")
		})
	}
}

func TestStoreConcurrentStateAndAccountWriters(t *testing.T) {
	store, root := storeTest(t)
	scope := storeTestScope()
	account := storeTestAccount(t, store, scope)
	const writers = 12
	start := make(chan struct{})
	stateResults := make(chan error, writers)
	var pending sync.WaitGroup
	for range writers {
		pending.Add(1)
		go func() {
			defer pending.Done()
			<-start
			_, err := NewStore(root).StateCAS(scope, "counter", 0, json.RawMessage(`1`))
			stateResults <- err
		}()
	}
	close(start)
	pending.Wait()
	close(stateResults)
	winners := 0
	for err := range stateResults {
		if err == nil {
			winners++
		} else {
			storeTestError(t, err, "conflict")
		}
	}
	if winners != 1 {
		t.Fatalf("state CAS winners = %d; want 1", winners)
	}
	start = make(chan struct{})
	accountResults := make(chan error, writers)
	for range writers {
		pending.Add(1)
		go func() {
			defer pending.Done()
			<-start
			_, err := NewStore(root).SaveAccount(account, 1)
			accountResults <- err
		}()
	}
	close(start)
	pending.Wait()
	close(accountResults)
	winners = 0
	for err := range accountResults {
		if err == nil {
			winners++
		} else {
			storeTestError(t, err, "conflict")
		}
	}
	if winners != 1 {
		t.Fatalf("account CAS winners = %d; want 1", winners)
	}
}

func TestStoreJournalTransitionsAndNoReplay(t *testing.T) {
	for _, terminal := range []string{"accepted", "uncertain", "completed", "not-started"} {
		t.Run(terminal, func(t *testing.T) {
			store, root := storeTest(t)
			scope := sdk.Scope{Plugin: "test-plugin", Operation: "../../operation", Session: "session", Attempt: 1}
			_, err := store.Journal(scope, terminal)
			storeTestError(t, err, "operation_conflict")
			dispatched, err := store.Journal(scope, "dispatched")
			if err != nil || dispatched.State != "dispatched" || dispatched.Updated.IsZero() {
				t.Fatalf("dispatch = %+v, %v", dispatched, err)
			}
			_, err = NewStore(root).Journal(scope, "dispatched")
			if failure := storeTestError(t, err, "operation_conflict"); failure.Outcome != "uncertain" {
				t.Fatalf("duplicate dispatch outcome = %q; want uncertain", failure.Outcome)
			}
			for _, changed := range []sdk.Scope{
				{Plugin: scope.Plugin, Operation: scope.Operation, Session: "other", Attempt: 1},
				{Plugin: scope.Plugin, Operation: scope.Operation, Session: scope.Session, Attempt: 2},
				{Plugin: scope.Plugin, Provider: "other", Operation: scope.Operation, Session: scope.Session, Attempt: 1},
			} {
				_, err = store.Journal(changed, terminal)
				storeTestError(t, err, "operation_conflict")
			}
			finished, err := store.Journal(scope, terminal)
			if err != nil || finished.State != terminal {
				t.Fatalf("transition = %+v, %v", finished, err)
			}
			persisted, err := NewStore(root).Operation(scope.Plugin, scope.Operation)
			if err != nil || persisted != finished {
				t.Fatalf("persisted journal = %+v, %v", persisted, err)
			}
			_, err = store.Journal(scope, "dispatched")
			storeTestError(t, err, "operation_conflict")
			if terminal == "accepted" || terminal == "uncertain" {
				_, err = store.Journal(scope, "not-started")
				if failure := storeTestError(t, err, "operation_conflict"); failure.Outcome != terminal {
					t.Fatalf("refused downgrade outcome = %q; want %q", failure.Outcome, terminal)
				}
				canceled, err := store.Journal(scope, "canceled")
				if err != nil || canceled.State != "uncertain" {
					t.Fatalf("canceled operation = %+v, %v", canceled, err)
				}
				_, err = store.Journal(scope, "accepted")
				storeTestError(t, err, "operation_conflict")
				if _, err := store.Journal(scope, "completed"); err != nil {
					t.Fatal(err)
				}
			}
			_, err = store.Operation("other-plugin", scope.Operation)
			storeTestError(t, err, "not_found")
		})
	}
}

func TestStoreConcurrentDispatchHasOneOwner(t *testing.T) {
	_, root := storeTest(t)
	scope := sdk.Scope{Plugin: "test-plugin", Operation: "same-operation"}
	results := make(chan error, 12)
	var pending sync.WaitGroup
	start := make(chan struct{})
	for range cap(results) {
		pending.Add(1)
		go func() {
			defer pending.Done()
			<-start
			_, err := NewStore(root).Journal(scope, "dispatched")
			results <- err
		}()
	}
	close(start)
	pending.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else {
			storeTestError(t, err, "operation_conflict")
		}
	}
	if winners != 1 {
		t.Fatalf("dispatch owners = %d; want 1", winners)
	}
}

func TestStoreReadErrorsPreserveBytes(t *testing.T) {
	for _, kind := range []string{"accounts", "state", "operation"} {
		for _, data := range []string{"{synthetic-private-secret", "null", "{}", `{"schema":1,"accounts":null}`, strings.Repeat("x", storeFileLimit+1)} {
			t.Run(fmt.Sprintf("%s/%d/%s", kind, len(data), data[:1]), func(t *testing.T) {
				store, root := storeTest(t)
				scope := storeTestScope()
				account := storeTestAccount(t, store, scope)
				if _, err := store.StateCAS(scope, "key", 0, json.RawMessage(`1`)); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Journal(scope, "dispatched"); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, "private", scope.Plugin, "accounts.json")
				switch kind {
				case "state":
					path = filepath.Join(root, "private", scope.Plugin, "state", storeStateName(scope, "key"))
				case "operation":
					path = filepath.Join(root, "private", scope.Plugin, "operations", storeHash(scope.Operation)+".json")
				}
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
				var readErr, writeErr error
				switch kind {
				case "accounts":
					_, readErr = store.Accounts(scope.Plugin, scope.Provider)
					_, writeErr = store.SaveAccount(account, account.Generation)
					_, err := store.StateGet(scope, "key")
					storeTestError(t, err, "store_unreadable")
					storeTestError(t, store.DeleteAccount(scope.Plugin, scope.Provider, scope.Account, 1), "store_unreadable")
				case "state":
					_, readErr = store.StateGet(scope, "key")
					_, writeErr = store.StateCAS(scope, "key", 1, json.RawMessage(`2`))
				case "operation":
					_, readErr = store.Operation(scope.Plugin, scope.Operation)
					_, writeErr = store.Journal(scope, "accepted")
				}
				storeTestError(t, readErr, "store_unreadable")
				storeTestError(t, writeErr, "store_unreadable")
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, []byte(data)) {
					t.Fatalf("bad source was changed: %v", err)
				}
			})
		}
	}
}

func TestStoreUnreadableFilesAreUnknown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission fixture")
	}
	store, root := storeTest(t)
	scope := storeTestScope()
	account := storeTestAccount(t, store, scope)
	path := filepath.Join(root, "private", scope.Plugin, "accounts.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Error(err)
		}
	})
	if probe, err := os.Open(path); err == nil {
		probe.Close()
		t.Skip("process can read mode-000 files")
	}
	_, err = store.Accounts(scope.Plugin, scope.Provider)
	storeTestError(t, err, "store_unreadable")
	_, err = store.GetAccount(scope.Plugin, scope.Provider, scope.Account)
	storeTestError(t, err, "store_unreadable")
	_, err = store.SaveAccount(account, 1)
	storeTestError(t, err, "store_unreadable")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("unreadable source changed: %v", err)
	}
}

func TestStoreRejectsSymlinkComponentsAndFiles(t *testing.T) {
	for _, component := range []string{"root", "ancestor", "private", "namespace", "state", "operations", "accounts.json", "state.json", "operation.json", ".lock"} {
		t.Run(component, func(t *testing.T) {
			store, root := storeTest(t)
			scope := sdk.Scope{Plugin: "test-plugin", Operation: "operation"}
			if _, err := store.StateCAS(scope, "key", 0, json.RawMessage(`1`)); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Journal(scope, "dispatched"); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "sentinel")
			saved := []byte("synthetic-private-secret")
			if err := os.WriteFile(sentinel, saved, 0o600); err != nil {
				t.Fatal(err)
			}
			path := root
			destination := outside
			switch component {
			case "ancestor":
				path = filepath.Join(filepath.Dir(root), "linked")
				store = NewStore(filepath.Join(path, "new-root"))
			case "private":
				path = filepath.Join(root, "private")
			case "namespace":
				path = filepath.Join(root, "private", scope.Plugin)
			case "state", "operations":
				path = filepath.Join(root, "private", scope.Plugin, component)
			case "accounts.json":
				path = filepath.Join(root, "private", scope.Plugin, component)
				destination = sentinel
			case "state.json":
				path = filepath.Join(root, "private", scope.Plugin, "state", storeStateName(scope, "key"))
				destination = sentinel
			case "operation.json":
				path = filepath.Join(root, "private", scope.Plugin, "operations", storeHash(scope.Operation)+".json")
				destination = sentinel
			case ".lock":
				path = filepath.Join(root, "private", ".lock")
				destination = sentinel
			}
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(destination, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			switch component {
			case "accounts.json":
				_, err := store.Accounts(scope.Plugin, "vendor")
				storeTestError(t, err, "store_unreadable")
				_, err = store.SaveAccount(Account{Plugin: scope.Plugin, Provider: "vendor", ID: "account", Auth: json.RawMessage(`{}`)}, 0)
				storeTestError(t, err, "store_unreadable")
			case "operations", "operation.json":
				_, err := store.Operation(scope.Plugin, scope.Operation)
				storeTestError(t, err, "store_unreadable")
				_, err = store.Journal(scope, "accepted")
				storeTestError(t, err, "store_unreadable")
			default:
				_, err := store.StateGet(scope, "key")
				storeTestError(t, err, "store_unreadable")
				_, err = store.StateCAS(scope, "key", 1, json.RawMessage(`2`))
				storeTestError(t, err, "store_unreadable")
			}
			after, err := os.ReadFile(sentinel)
			if err != nil || !bytes.Equal(after, saved) {
				t.Fatalf("symlink target changed: %v", err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 1 {
				t.Fatalf("files created outside root: %v, %v", entries, err)
			}
		})
	}
}

func TestStorePrivateAtomicFilesAndBoundedInput(t *testing.T) {
	store, root := storeTest(t)
	scope := sdk.Scope{Plugin: "test-plugin", Operation: "../../operation"}
	first, err := store.StateCAS(scope, "../key", 0, json.RawMessage(`1`))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "private", scope.Plugin, "state", storeStateName(scope, "../key"))
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	os.SameFile(before, before)
	if _, err := store.StateCAS(scope, "../key", first.Version, json.RawMessage(`2`)); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || os.SameFile(before, after) {
		t.Fatalf("state was not replaced atomically: %v", err)
	}
	exact := json.RawMessage(`"` + strings.Repeat("<", storeStateLimit-2) + `"`)
	if _, err := store.StateCAS(scope, "limit", 0, exact); err != nil {
		t.Fatalf("exact JSON limit: %v", err)
	}
	for _, invalid := range []json.RawMessage{nil, json.RawMessage(`{`), append(bytes.Clone(exact), ' ')} {
		_, err := store.StateCAS(scope, "../key", 2, invalid)
		storeTestError(t, err, "invalid_state")
	}
	if _, err := store.Journal(scope, "dispatched"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveAccount(Account{Plugin: scope.Plugin, Provider: "../../vendor", ID: "../../account", Auth: json.RawMessage(`{}`)}, 0); err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(filepath.Join(root, "private"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if entry.IsDir() {
			want = 0o700
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != want {
			t.Errorf("%s permissions = %v; want %v", path, info.Mode().Perm(), want)
		}
		if strings.HasPrefix(entry.Name(), ".store-") {
			t.Errorf("temporary file remains: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.StateGet(scope, "../key")
	if err != nil || got.Version != 2 || string(got.Value) != "2" {
		t.Fatalf("invalid write damaged source: %+v, %v", got, err)
	}
}

func TestStoreAcceptsPublicPluginIDs(t *testing.T) {
	for _, plugin := range []string{"0", "0vendor", "vendor.plugin", "vendor_plugin", "trailing-", "trailing.", strings.Repeat("a", 128)} {
		t.Run(plugin, func(t *testing.T) {
			store, root := storeTest(t)
			scope := sdk.Scope{Plugin: plugin, Operation: "operation"}
			state, err := store.StateCAS(scope, "key", 0, json.RawMessage(`1`))
			if err != nil || state.Version != 1 {
				t.Fatalf("public ID state = %+v, %v", state, err)
			}
			persisted, err := NewStore(root).StateGet(scope, "key")
			if err != nil || persisted.Version != 1 || string(persisted.Value) != "1" {
				t.Fatalf("persisted public ID state = %+v, %v", persisted, err)
			}
			account := storeTestAccount(t, store, sdk.Scope{Plugin: plugin, Provider: "vendor", Account: "account"})
			got, err := store.GetAccount(plugin, account.Provider, account.ID)
			if err != nil || got.Generation != account.Generation {
				t.Fatalf("public ID account = %+v, %v", got, err)
			}
			if _, err := store.Journal(scope, "dispatched"); err != nil {
				t.Fatal(err)
			}
			entry, err := NewStore(root).Operation(plugin, scope.Operation)
			if err != nil || entry.State != "dispatched" || entry.Scope != scope {
				t.Fatalf("public ID operation = %+v, %v", entry, err)
			}
		})
	}
}

func TestStoreInvalidScopesAndTokens(t *testing.T) {
	store, root := storeTest(t)
	for _, plugin := range []string{"", ".", "..", "../outside", "Uppercase", "white space", "a/b", "a\\b", "/absolute", "-leading", "_leading", ".leading", "null\x00id", "id\nline", "模块", strings.Repeat("a", 129)} {
		scope := sdk.Scope{Plugin: plugin}
		_, err := store.StateCAS(scope, "key", 0, json.RawMessage(`1`))
		storeTestError(t, err, "invalid_scope")
		_, err = store.Accounts(plugin, "vendor")
		storeTestError(t, err, "invalid_scope")
	}
	for _, scope := range []sdk.Scope{
		{Plugin: "plugin", Account: "account", Generation: 1},
		{Plugin: "plugin", Provider: "vendor", Account: "account"},
		{Plugin: "plugin", Generation: 1},
		{Plugin: "plugin", Operation: strings.Repeat("x", 257)},
		{Plugin: "plugin", Session: "line\nbreak"},
	} {
		_, err := store.StateGet(scope, "key")
		storeTestError(t, err, "invalid_scope")
	}
	for _, token := range []string{"", strings.Repeat("x", 257), "null\x00token", string([]byte{0xff})} {
		_, err := store.Journal(sdk.Scope{Plugin: "plugin", Operation: token}, "dispatched")
		storeTestError(t, err, "invalid_scope")
		_, err = store.Operation("plugin", token)
		storeTestError(t, err, "invalid_scope")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid input created a store: %v", err)
	}
}

func TestStoreAccountSizeLimitPreservesSource(t *testing.T) {
	store, root := storeTest(t)
	scope := storeTestScope()
	account := storeTestAccount(t, store, scope)
	path := filepath.Join(root, "private", scope.Plugin, "accounts.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	account.Auth = json.RawMessage(`"` + strings.Repeat("x", storeFileLimit-2) + `"`)
	_, err = store.SaveAccount(account, account.Generation)
	storeTestError(t, err, "invalid_state")
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("size rejection damaged account source: %v", err)
	}
}

func TestStoreVersionAndGenerationCannotWrap(t *testing.T) {
	store, root := storeTest(t)
	scope := storeTestScope()
	account := storeTestAccount(t, store, scope)
	if _, err := store.StateCAS(scope, "key", 0, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	state := storeState{
		Scope: storeStateScope(scope), Key: "key",
		VersionedValue: VersionedValue{Version: ^uint64(0), Value: json.RawMessage(`1`)},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "private", scope.Plugin, "state", storeStateName(scope, "key"))
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.StateCAS(scope, "key", ^uint64(0), json.RawMessage(`2`))
	storeTestError(t, err, "conflict")
	account.Generation = ^uint64(0)
	records := storeAccounts{Schema: 1, Accounts: []storeAccount{{Account: account}}}
	data, err = json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	accountPath := filepath.Join(root, "private", scope.Plugin, "accounts.json")
	if err := os.WriteFile(accountPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveAccount(account, account.Generation)
	storeTestError(t, err, "conflict")
	storeTestError(t, store.DeleteAccount(scope.Plugin, scope.Provider, scope.Account, account.Generation), "conflict")
	got, err := store.GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil || got.Generation != ^uint64(0) {
		t.Fatalf("generation wrapped: %+v, %v", got, err)
	}
}
