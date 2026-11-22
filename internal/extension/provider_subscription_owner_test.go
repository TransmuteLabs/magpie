package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSubscriptionOwnersMissingIsReadOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	rows, err := NewStore(root).SubscriptionOwners(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty owners: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created the extension store")
	}
}

func TestSavedSubscriptionCompleteSnapshotAndRollback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	ctx := t.Context()
	path := t.TempDir()
	m := NewManager(path)
	target := ProviderSignIn{Module: "removed-module", Provider: "vendor", Revision: strings.Repeat("b", 64)}
	keys := []string{"vendor", "vendor#second"}
	tickets := []string{"first", "second"}
	legacy := map[string]json.RawMessage{}
	var accounts []SubscriptionAccount
	var saved []savedSignIn
	for i, key := range keys {
		now := time.Now()
		c := testSavedSignIn(tickets[i], now.Add(time.Minute), now.Add(time.Hour))
		c.Key = key
		legacy[key] = json.RawMessage(`{"synthetic":true}`)
		c.Digest = savedSignInDigest(legacy[key])
		saved = append(saved, c)
		accounts = append(accounts, SubscriptionAccount{Key: key, User: "legacy-" + tickets[i], LoginID: "login-" + tickets[i]})
	}
	writeTickets := func() {
		t.Helper()
		if err := withTestSavedSignIns(m.store, ctx, func(dir *os.Root) error { return writeSavedSignIns(dir, saved) }); err != nil {
			t.Fatal(err)
		}
	}
	var checkSource func() error
	call := func() (SubscriptionOwner, error) {
		var owner SubscriptionOwner
		err := m.store.withPrivateStore(ctx, func(private *os.Root) error {
			dir, err := storeDirectory(private, savedSignInsDir)
			if err != nil {
				return err
			}
			defer dir.Close()
			root, err := storeDirectory(private, target.Module)
			if err != nil {
				return err
			}
			defer root.Close()
			owner, err = publishSubscription(ctx, dir, root, target, target.Provider, tickets, legacy, accounts, checkSource)
			return err
		})
		return owner, err
	}
	source := func(_ context.Context, _ string, use func(map[string]json.RawMessage, []SubscriptionAccount, func() error) error) error {
		return use(legacy, accounts, checkSource)
	}
	writeTickets()
	legacy["vendor#new"] = json.RawMessage(`{}`)
	if _, err := call(); err == nil {
		t.Fatal("extra legacy account was ignored")
	}
	delete(legacy, "vendor#new")
	accounts[1].LoginID = accounts[0].LoginID
	if _, err := call(); err == nil {
		t.Fatal("duplicate public ACL identity accepted")
	}
	accounts[1].LoginID = "login-second"
	if observed, err := m.store.Accounts(target.Module, target.Provider); err != nil || len(observed) != 0 {
		t.Fatal("failed preflight published accounts")
	}
	if err := settings.Save(settings.Settings{NoAccountDiscovery: true}); err != nil {
		t.Fatal(err)
	}
	if refused, err := call(); err == nil || refused.Revision != "" {
		t.Error("transfer published an owner after discovery was disabled")
	}
	if owners, err := m.store.SubscriptionOwners(ctx); err != nil || len(owners) != 0 {
		t.Error("refused transfer left an owner")
	}
	if records, err := m.store.Accounts(target.Module, target.Provider); err != nil || len(records) != 0 {
		t.Error("refused transfer wrote credentials")
	}
	if err := settings.Save(settings.Settings{}); err != nil {
		t.Fatal(err)
	}
	authorityChanged := errors.New("source authority changed")
	checkSource = func() error { return authorityChanged }
	if refused, err := call(); !errors.Is(err, authorityChanged) || refused.Revision != "" {
		t.Errorf("reservation ignored the final source check: %v", err)
	}
	if owners, err := m.store.SubscriptionOwners(ctx); err != nil || len(owners) != 0 {
		t.Error("refused reservation left an owner")
	}
	checks := 0
	checkSource = func() error {
		checks++
		if checks == 2 {
			return authorityChanged
		}
		return nil
	}
	pending, err := call()
	if !errors.Is(err, authorityChanged) || !pending.Pending || pending.Active || pending.Revision == "" {
		t.Fatalf("activation ignored the final source check or lost its pending receipt: %v", err)
	}
	if owners, err := m.store.SubscriptionOwners(ctx); err != nil || len(owners) != 1 || owners[0].Active ||
		!owners[0].Pending || owners[0].Revision != pending.Revision {
		t.Fatal("refused activation changed the pending owner")
	}
	checkSource = nil
	first, err := call()
	if err != nil || !first.Active || first.Accounts[1].ID != "second" {
		t.Fatalf("activation: %v %+v", err, first)
	}
	// Simulate a process that durably reserved the pair and wrote only the
	// first account before it died. Recovery uses the exact staged bytes.
	err = m.store.withPrivateStore(ctx, func(private *os.Root) error {
		dir, err := storeDirectory(private, savedSignInsDir)
		if err != nil {
			return err
		}
		defer dir.Close()
		rows, err := readSubscriptionRecords(dir)
		if err != nil {
			return err
		}
		rows[0].Active = false
		rows[0].Pending = true
		if err := storeWrite(dir, subscriptionOwnersFile, rows, storeFileLimit); err != nil {
			return err
		}
		root, err := storeDirectory(private, target.Module)
		if err != nil {
			return err
		}
		defer root.Close()
		rec, err := storeReadAccounts(root, target.Module)
		if err != nil {
			return err
		}
		rec.Accounts = rec.Accounts[:1]
		return storeWrite(root, "accounts.json", rec, storeFileLimit)
	})
	if err != nil {
		t.Fatal(err)
	}
	writeTickets()
	hidden, err := NewStore(path).SubscriptionOwners(ctx)
	if err != nil || len(hidden) != 1 || hidden[0].Active {
		t.Fatal("partial publication exposed the provider")
	}
	first, err = call()
	if err != nil || !first.Active {
		t.Fatalf("resume partial activation: %v", err)
	}
	records, err := m.store.Accounts(target.Module, target.Provider)
	if err != nil || len(records) != 2 || records[0].Generation != 1 || records[1].Generation != 1 {
		t.Fatal("partial publication repeated a credential generation")
	}
	again, err := call()
	if err != nil || again.Revision != first.Revision {
		t.Fatalf("activation replay: %v", err)
	}
	before, err := accountStampAt(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy[keys[1]] = json.RawMessage(`{}`)
	if _, err := m.RollbackSavedSubscription(ctx, target.Provider, first.Revision, source); err == nil {
		t.Fatal("changed source allowed rollback")
	}
	legacy[keys[1]] = legacy[keys[0]]
	staleSource := func(_ context.Context, _ string, use func(map[string]json.RawMessage, []SubscriptionAccount, func() error) error) error {
		if err := os.WriteFile(settings.Path(), []byte(`{"noAccountDiscovery":true}`), 0o600); err != nil {
			return err
		}
		return use(legacy, accounts, nil)
	}
	if refused, err := m.RollbackSavedSubscription(ctx, target.Provider, first.Revision, staleSource); err == nil || refused.Revision != "" {
		t.Error("rollback published an owner after discovery was disabled")
	}
	if owners, err := m.store.SubscriptionOwners(ctx); err != nil || len(owners) != 1 || owners[0].Revision != first.Revision {
		t.Error("refused rollback changed the active owner")
	}
	if err := settings.Save(settings.Settings{}); err != nil {
		t.Fatal(err)
	}
	checkSource = func() error { return authorityChanged }
	if refused, err := m.RollbackSavedSubscription(ctx, target.Provider, first.Revision, source); !errors.Is(err, authorityChanged) || refused.Revision != "" {
		t.Errorf("rollback ignored the final source check: %v", err)
	}
	if owners, err := m.store.SubscriptionOwners(ctx); err != nil || len(owners) != 1 || !owners[0].Active || owners[0].Revision != first.Revision {
		t.Fatal("refused rollback changed the active owner")
	}
	checkSource = nil
	rolled, err := m.RollbackSavedSubscription(ctx, target.Provider, first.Revision, source)
	if err != nil || rolled.Active || rolled.Revision == first.Revision {
		t.Fatalf("rollback: %v", err)
	}
	after, err := accountStampAt(path)
	if err != nil || before.Equal(after) {
		t.Fatal("rollback did not invalidate cross-process discovery")
	}
	legacy[keys[1]] = json.RawMessage(`{"legacy-refreshed":true}`)
	checkSource = func() error { return authorityChanged }
	if replay, err := m.RollbackSavedSubscription(ctx, target.Provider, first.Revision, source); err != nil || replay.Revision != rolled.Revision {
		t.Fatal("rollback replay changed revision")
	}
	checkSource = nil
	legacy[keys[1]] = legacy[keys[0]]
	if _, err := call(); err == nil {
		t.Fatal("stale tickets reactivated rolled-back subscription")
	}
	for i := range saved {
		saved[i].OwnerRevision = rolled.Revision
		saved[i].Ticket += "new"
		tickets[i] = saved[i].Ticket
	}
	writeTickets()
	next, err := call()
	if err != nil || next.Revision == first.Revision || !next.Active {
		t.Fatalf("reactivate: %v", err)
	}
	if err := m.store.DeleteAccount(target.Module, target.Provider, "second", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := call(); err == nil {
		t.Fatal("activation replay accepted a deleted account")
	}
	if _, err := m.RollbackSavedSubscription(ctx, target.Provider, next.Revision, source); err == nil {
		t.Fatal("rollback would restore a signed-out account")
	}
	got, err := m.store.Accounts(target.Module, target.Provider)
	if err != nil || len(got) != 1 || got[0].ID != "first" {
		t.Fatal("rollback resurrected an account")
	}
	owners, err := NewStore(path).SubscriptionOwners(ctx)
	if err != nil || len(owners) != 1 || !owners[0].Active || owners[0].Revision != next.Revision ||
		!slices.Equal(owners[0].Accounts, first.Accounts) {
		t.Fatalf("refused rollback changed the active owner: %v", err)
	}
}

func TestSavedSubscriptionRollbackChecksRefreshAfterRestart(t *testing.T) {
	for _, state := range []string{"dispatched", "not-started", "completed", "uncertain"} {
		t.Run(state, func(t *testing.T) {
			ctx := t.Context()
			path := t.TempDir()
			manager := NewManager(path)
			now := time.Now()
			candidate := testSavedSignIn("first", now.Add(time.Minute), now.Add(time.Hour))
			target := ProviderSignIn{Module: candidate.Module, Provider: candidate.Provider, Revision: candidate.Revision}
			raw := map[string]json.RawMessage{candidate.Key: json.RawMessage(`{"synthetic":true}`)}
			candidate.Digest = savedSignInDigest(raw[candidate.Key])
			accounts := []SubscriptionAccount{{Key: candidate.Key, User: "community-first", LoginID: "login-first"}}
			var owner SubscriptionOwner
			if err := manager.store.withPrivateStore(ctx, func(private *os.Root) error {
				dir, err := storeDirectory(private, savedSignInsDir)
				if err != nil {
					return err
				}
				defer dir.Close()
				root, err := storeDirectory(private, target.Module)
				if err != nil {
					return err
				}
				defer root.Close()
				if err := writeSavedSignIns(dir, []savedSignIn{candidate}); err != nil {
					return err
				}
				owner, err = publishSubscription(ctx, dir, root, target, target.Provider, []string{candidate.Ticket}, raw, accounts, nil)
				return err
			}); err != nil || !owner.Active {
				t.Fatalf("seed transferred subscription: %v", err)
			}
			scope := sdk.Scope{
				Plugin: target.Module, Provider: target.Provider, Account: candidate.Result.ID,
				Generation: candidate.Generation + 1, Operation: "refresh-operation", Attempt: 1,
			}
			if _, err := manager.store.claimRefresh(scope, target.Revision, "0"); err != nil {
				t.Fatal(err)
			}
			if state != "dispatched" {
				var replacement *sdk.AuthResult
				if state == "completed" {
					replacement = &sdk.AuthResult{ID: scope.Account, Credentials: json.RawMessage(`{"synthetic":"replacement"}`)}
				}
				if _, err := manager.store.finishRefresh(scope, replacement, state); err != nil {
					t.Fatal(err)
				}
			}
			reopened := NewManager(path)
			source := func(_ context.Context, _ string, use func(map[string]json.RawMessage, []SubscriptionAccount, func() error) error) error {
				return use(raw, accounts, nil)
			}
			rolled, err := reopened.RollbackSavedSubscription(ctx, owner.PublicID, owner.Revision, source)
			if state == "not-started" {
				if err != nil || rolled.Active || rolled.Revision == owner.Revision {
					t.Fatalf("rollback after a refused refresh: %v", err)
				}
				return
			}
			if err == nil || rolled.Revision != "" {
				t.Fatal("rollback reused community credentials after an unsafe refresh")
			}
			rows, err := reopened.store.SubscriptionOwners(ctx)
			if err != nil || len(rows) != 1 || !rows[0].Active || rows[0].Revision != owner.Revision {
				t.Fatalf("unsafe rollback changed the owner: %v", err)
			}
		})
	}
}

func TestProviderAuthChecksOwnerInsideCatalogAuthorityAndAfterRestart(t *testing.T) {
	path := t.TempDir()
	m := NewManager(path)
	manifest := authfixture.Manifest()
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.ProviderSignIn)
	manifest.AuthBindings[0].From = sdk.ProviderSignInPart
	for i := range manifest.AuthBindings[0].Methods {
		manifest.AuthBindings[0].Methods[i].Label = manifest.AuthBindings[0].Methods[i].ID
	}
	manifest.Window = nil
	src := t.TempDir()
	catalogTestWriteManifest(t, src, manifest)
	catalogTestWrite(t, filepath.Join(src, manifest.Entry), []byte("inert fixture"), 0700)
	entry, err := m.catalog.Install(t.Context(), src, func(context.Context, Installed) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx := authWindowContext(t.Context(), "request")
	part, rev := windowAuthJournalSelection(entry, "0", sdk.ProviderSignInPart)
	scope := sdk.Scope{Plugin: manifest.ID, Provider: "vendor", Account: "alice", Generation: 1}
	if _, err := m.store.SaveAccount(Account{Plugin: manifest.ID, Provider: "vendor", ID: "alice", Auth: json.RawMessage(`{"fixture":true}`)}, 0); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	record := windowAuthRecord{Part: part, Revision: rev, Config: "0", Scope: scope, Binding: "signin", Flow: storeHash("flow"), Intent: storeHash("intent"), Fingerprint: storeHash("fingerprint"), Parent: "parent-frame", Method: "browser", Created: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), Phase: "reserved"}
	if err := m.withWindowAuthAuthority(ctx, record, func() error { _, _, err := m.store.reserveWindowAuth(record); return err }); err != nil {
		t.Fatal(err)
	}
	current := subscriptionRecord{SubscriptionOwner: SubscriptionOwner{PublicID: "vendor", Provider: "vendor", Module: manifest.ID, Revision: "revision-one", Active: true, Accounts: []SubscriptionAccount{{Key: "vendor", ID: "alice", User: "Alice legacy", LoginID: "first"}}}, GrantRevision: rev, Digests: map[string]string{"vendor": strings.Repeat("a", 64)}, Tickets: []string{"ticket"}}
	setOwner := func() error {
		return withTestSavedSignIns(m.store, t.Context(), func(dir *os.Root) error {
			return storeWrite(dir, subscriptionOwnersFile, []subscriptionRecord{current}, storeFileLimit)
		})
	}
	stale := WithWindowAuthPolicy(ctx, func(c context.Context, _ sdk.Scope) (context.Context, error) { return c, setOwner() })
	admitted := false
	if err := m.withWindowAuthAuthority(stale, record, func() error { admitted = true; return nil }); err == nil || admitted {
		t.Fatal("ownership changed after policy check but stale publication was admitted")
	}
	reopened := NewManager(path)
	if _, err := reopened.CoreAuthFinish(ctx, manifest.ID, rev, sdk.ProviderSignInPart, "signin", record.Flow); err == nil {
		t.Fatal("stale unowned receipt replayed after restart")
	}
	owned := WithSubscriptionOwnerRevision(ctx, current.Revision)
	record.Revision = subscriptionJournalRevision(owned, rev)
	if err := m.withWindowAuthAuthority(owned, record, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "alice", "new-id"} {
		candidate := scope
		candidate.Account = id
		if err := m.store.checkSubscriptionView(owned, candidate); err != nil {
			t.Fatalf("account %q: %v", id, err)
		}
	}
	for _, id := range []string{"ALICE", "alice LEGACY", "FIRST"} {
		candidate := scope
		candidate.Account = id
		if err := m.store.checkSubscriptionView(owned, candidate); err == nil {
			t.Fatalf("accepted colliding %q", id)
		}
	}
	if err := m.DeleteSubscriptionAccount(ctx, scope, "old-revision"); err == nil {
		t.Fatal("stale logout succeeded")
	}
	if err := m.DeleteSubscriptionAccount(ctx, scope, current.Revision); err != nil {
		t.Fatal(err)
	}
	other := Account{Plugin: scope.Plugin, Provider: scope.Provider, ID: "new-id", Auth: json.RawMessage(`{"fixture":true}`)}
	saved, err := m.store.SaveAccount(other, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.store.DeleteAccount(other.Plugin, other.Provider, other.ID, saved.Generation); err != nil {
		t.Fatal(err)
	}
	if err := m.store.checkSubscriptionResult(owned, scope, "New-ID"); err == nil {
		t.Fatal("Add reclaimed another signed-out account")
	}
	if err := m.store.checkSubscriptionResult(owned, scope, "alice"); err != nil {
		t.Fatal(err)
	}
	current.Revision = "revision-two"
	current.Active = false
	if err := setOwner(); err != nil {
		t.Fatal(err)
	}
	if err := m.withWindowAuthAuthority(owned, record, func() error { return nil }); err == nil {
		t.Fatal("rolled-back owner admitted a waiting flow")
	}
}
