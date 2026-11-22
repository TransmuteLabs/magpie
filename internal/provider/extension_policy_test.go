package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func writeExtensionPolicyFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func registerFixtureExtensionPolicies(t *testing.T, host extensionProviderHost, module, provider string) {
	t.Helper()
	accounts, err := host.Store().Accounts(module, provider)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if err := registerExtensionLogin(t.Context(), host, sdk.Scope{Plugin: module, Provider: provider,
			Account: account.ID, Generation: account.Generation}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExtensionPolicyDiscoveryLeavesLoginsAlone(t *testing.T) {
	for _, condition := range []string{"missing", "null", "empty", "malformed", "recovered"} {
		t.Run(condition, func(t *testing.T) {
			host := fixtureExtensionProvider(t, true)
			id := ExtensionProviderID(host.manifest.ID, "vendor")
			path := loginsPath()
			main, backup := "", ""
			switch condition {
			case "null":
				main = "null\n"
			case "empty":
				main = "[]\n"
			case "malformed", "recovered":
				main = `[{"on":"bad"}]`
				if condition == "recovered" {
					backup = `[{"agent":"` + id + `","user":"bob","on":true,"paused":true,"order":1,"plan":"Pro"}]`
				}
			}
			if main != "" {
				writeExtensionPolicyFixture(t, path, main)
			}
			if backup != "" {
				writeExtensionPolicyFixture(t, path+".bak", backup)
			}
			providers, err := ExtensionProviders()
			if condition == "malformed" {
				if err == nil || len(providers) != 0 {
					t.Fatalf("malformed policy projected %d providers, error = %v", len(providers), err)
				}
			} else if err != nil || len(providers) != 2 {
				t.Fatalf("read projected %d accounts, error = %v", len(providers), err)
			}
			logins := Logins(id)
			if condition == "recovered" {
				if providers[0].Account.User != "bob" || providers[0].Account.Plan != "Pro" ||
					len(logins) != 2 || logins[0].User != "bob" || logins[0].On || !logins[0].Paused || !logins[1].Active {
					t.Fatalf("recovered order and pause missing: %+v", logins)
				}
			} else if condition != "malformed" &&
				(len(logins) != 2 || logins[0].User != "alice" || !logins[0].Active || !logins[1].On || logins[1].Active) {
				t.Fatalf("default precedence missing: %+v", logins)
			}
			for path, want := range map[string]string{path: main, path + ".bak": backup, path + ".lock": ""} {
				got, err := os.ReadFile(path)
				if want == "" && !errors.Is(err, os.ErrNotExist) || want != "" && (err != nil || string(got) != want) {
					t.Errorf("ordinary read changed %s: %q, %v", filepath.Base(path), got, err)
				}
			}
			if copies, err := filepath.Glob(path + ".bad-*"); err != nil || len(copies) != 0 {
				t.Fatalf("ordinary read backed up malformed logins: %v, %v", copies, err)
			}
		})
	}
}

func TestExtensionPolicyMutationsRequireLoginLock(t *testing.T) {
	for _, action := range []string{"pause", "switch", "order", "register"} {
		t.Run(action, func(t *testing.T) {
			host := fixtureExtensionProvider(t, true)
			id := ExtensionProviderID(host.manifest.ID, "vendor")
			path := loginsPath()
			before := `[{"agent":"` + id + `","user":"alice","on":true,"order":1},{"agent":"` + id + `","user":"bob","on":true,"order":2}]`
			writeExtensionPolicyFixture(t, path, before)
			if err := os.Mkdir(path+".lock", 0o700); err != nil {
				t.Fatal(err)
			}
			var err error
			switch action {
			case "pause":
				err = SetLoginOn(id, "alice", false)
			case "switch":
				err = SwitchLogin(id, "bob")
			case "order":
				err = SetAccountOrder(id, []string{"bob", "alice"})
			case "register":
				err = registerExtensionLogin(t.Context(), host, sdk.Scope{
					Plugin: host.manifest.ID, Provider: "vendor", Account: "alice", Generation: 1,
				})
			}
			got, readErr := os.ReadFile(path)
			if err == nil || readErr != nil || string(got) != before {
				t.Fatalf("%s without the lock changed account policy: err=%v, read err=%v, bytes=%q", action, err, readErr, got)
			}
			if action == "register" {
				accounts, err := host.store.Accounts(host.manifest.ID, "vendor")
				if err != nil || len(accounts) != 2 || accounts[0].Generation != 1 || accounts[1].Generation != 1 {
					t.Fatalf("registration bypassed policy lock: %d accounts, %v", len(accounts), err)
				}
			}
		})
	}
}

func TestRegisterExtensionLoginUsesCommittedGeneration(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	account, err := host.store.GetAccount(host.manifest.ID, "vendor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: account.Plugin, Provider: account.Provider, Account: account.ID, Generation: account.Generation}
	updated, err := host.store.SaveAccount(account, account.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterExtensionLogin(t.Context(), scope); err == nil {
		t.Fatal("registration accepted an obsolete credential generation")
	}
	if _, err := os.Stat(loginsPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("obsolete generation changed the policy file: %v", err)
	}
	scope.Generation = updated.Generation
	if err := RegisterExtensionLogin(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	saved, err := readLoginsFile()
	id := ExtensionProviderID(scope.Plugin, scope.Provider)
	if err != nil || len(saved) != 1 || saved[0].Agent != id || saved[0].User != scope.Account || !saved[0].On || string(saved[0].Auth) != "null" {
		t.Fatalf("registration did not save only the committed account policy: %d rows, %v", len(saved), err)
	}
	if err := SetLoginOn(id, scope.Account, false); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(loginsPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterExtensionLogin(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(loginsPath())
	if err != nil || !slices.Equal(before, after) {
		t.Fatalf("repeat registration changed existing disabled policy: %v", err)
	}
}

func TestExtensionPolicyFirstEditsPreserveOrderAndPause(t *testing.T) {
	for _, action := range []string{"switch", "order"} {
		t.Run(action, func(t *testing.T) {
			host := fixtureExtensionProvider(t, true)
			id := ExtensionProviderID(host.manifest.ID, "vendor")
			before := `[{"agent":"` + id + `","user":"alice","on":true,"paused":true,"order":1,"plan":"Pro"}]`
			writeExtensionPolicyFixture(t, loginsPath(), before)
			var err error
			if action == "switch" {
				err = SwitchLogin(id, "bob")
			} else {
				err = SetAccountOrder(id, []string{"bob", "alice"})
			}
			if err != nil {
				t.Fatal(err)
			}
			logins := Logins(id)
			if len(logins) != 2 || !logins[0].Active || logins[0].User != "bob" ||
				logins[1].User != "alice" || logins[1].On || !logins[1].Paused || logins[1].Plan != "Pro" {
				t.Fatalf("first %s lost policy: %+v", action, logins)
			}
		})
	}
}

func TestExtensionPolicyFirstEditReadsLatestLoginFile(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	id := ExtensionProviderID(host.manifest.ID, "vendor")
	path := loginsPath()
	writeExtensionPolicyFixture(t, path, "[]\n")
	unlock, held := filelock.Lock(path+".lock", time.Second)
	if !held {
		t.Fatal("could not lock the fixture policy")
	}
	release := sync.OnceFunc(unlock)
	defer release()
	trying := make(chan struct{})
	signal := sync.OnceFunc(func() { close(trying) })
	lockPath := loginsMu.Path
	loginsMu.Path = func() string {
		signal()
		return lockPath()
	}
	done := make(chan struct{})
	var editErr error
	go func() {
		defer close(done)
		editErr = SetLoginOn(id, "bob", false)
	}()
	defer func() {
		release()
		<-done
		loginsMu.Path = lockPath
	}()
	select {
	case <-trying:
	case <-time.After(2 * time.Second):
		t.Fatal("first policy edit did not try to take the login lock")
	}
	writeExtensionPolicyFixture(t, path, `[{"agent":"`+id+`","user":"alice","on":true,"paused":true,"order":2,"plan":"Business"}]`)
	release()
	<-done
	if editErr != nil {
		t.Fatal(editErr)
	}
	logins := Logins(id)
	if len(logins) != 2 || logins[0].User != "alice" || logins[0].On ||
		!logins[0].Paused || logins[0].Plan != "Business" || logins[1].User != "bob" || logins[1].On {
		t.Fatalf("first edit wrote over a concurrent pause or rank: %+v", logins)
	}
}

func TestExtensionUsageDiscoveryDoesNotRepeatPerProvider(t *testing.T) {
	for _, condition := range []string{"all", "targeted", "hidden", "failed module"} {
		t.Run(condition, func(t *testing.T) {
			host, first, second := fixtureExtensionDiscovery(t)
			ctx := t.Context()
			hidden := map[string]bool{}
			wantProviders, wantReads := []string{first, second}, 3
			slices.Sort(wantProviders)
			switch condition {
			case "targeted":
				ctx = context.WithValue(ctx, cardRefreshKey{}, cardRefresh{provider: second, user: "carol"})
				wantProviders, wantReads = []string{second}, 1
			case "hidden":
				hidden[second] = true
				wantProviders, wantReads = []string{first}, 2
			case "failed module":
				host.configError = host.manifest.ID
				wantProviders, wantReads = []string{second}, 1
			}
			var requested []string
			fetches := extensionUsageFetches(ctx, func(id string) context.Context {
				requested = append(requested, id)
				return ctx
			}, hidden)
			if len(fetches) != wantReads {
				t.Errorf("usage planned %d account reads, want %d", len(fetches), wantReads)
			}
			if condition != "targeted" && !slices.Equal(requested, wantProviders) {
				t.Errorf("usage planned providers %v, want %v", requested, wantProviders)
			}
			if calls := host.listCalls.Load(); calls != 1 {
				t.Errorf("usage planning enumerated the SDK catalog %d times, want once", calls)
			}
		})
	}
}
