package extension

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func authStoreFixture(t *testing.T) (*Store, windowAuthRecord) {
	t.Helper()
	store := NewStore(t.TempDir())
	created := time.Now()
	record := windowAuthRecord{
		Flow: storeHash("flow"), Intent: storeHash("intent"), Fingerprint: storeHash("input"),
		Parent: "parent-frame", Revision: strings.Repeat("a", 64), Config: "0",
		Part: "window:page", Binding: "signin", Method: "device",
		Scope: sdk.Scope{Plugin: "fixture-auth", Provider: "vendor"},
		Phase: "reserved", Created: created.UnixMilli(), Expires: created.Add(10 * time.Minute).UnixMilli(),
	}
	return store, record
}

func readyAuthStore(t *testing.T, store *Store, record windowAuthRecord) {
	t.Helper()
	if _, created, err := store.reserveWindowAuth(record); err != nil || !created {
		t.Fatal("reserve:", created, err)
	}
	if _, err := store.updateWindowAuth(record.Scope.Plugin, record.Flow, func(value *windowAuthRecord, _ *storeAccounts) error {
		value.Phase = "polling"
		value.Interval, value.Next = 1000, 1
		value.Continuation = json.RawMessage(`{"secret":"private-continuation"}`)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWindowAuthStorePublishesAccountAndReceiptOnce(t *testing.T) {
	store, record := authStoreFixture(t)
	readyAuthStore(t, store, record)
	secret := json.RawMessage(`{"token":"private-account-secret"}`)
	var published atomic.Int32
	var writers sync.WaitGroup
	for range 8 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			owner := NewStore(store.root)
			result, err := owner.publishWindowAuth(t.Context(), record.Scope.Plugin, record.Flow, sdk.AuthResult{ID: "chosen", Credentials: secret}, nil)
			if err == nil {
				published.Add(1)
				if result.Receipt == nil || result.Receipt.Generation != "1" {
					t.Error("publication omitted committed generation")
				}
			}
		}()
	}
	writers.Wait()
	if published.Load() != 1 {
		t.Fatalf("published credential exchange %d times", published.Load())
	}
	reopened := NewStore(store.root)
	account, err := reopened.GetAccount(record.Scope.Plugin, "vendor", "chosen")
	if err != nil || account.Generation != 1 || !bytes.Equal(account.Auth, secret) {
		t.Fatal("account publication:", account.Generation, err)
	}
	receipt, err := reopened.windowAuthRecord(record.Scope.Plugin, record.Flow)
	if err != nil || receipt.Phase != "complete" || receipt.Receipt == nil || len(receipt.Continuation) != 0 {
		t.Fatal("receipt publication:", receipt.Phase, err)
	}
	public, err := json.Marshal(receipt.public())
	if err != nil || bytes.Contains(public, []byte("private")) || bytes.Contains(public, []byte("token")) {
		t.Fatalf("private authentication data reached public receipt: %s, %v", public, err)
	}
}

func TestWindowAuthStoreRejectsChangedIntentAndAccountAuthority(t *testing.T) {
	store, record := authStoreFixture(t)
	readyAuthStore(t, store, record)
	changed := record
	changed.Flow, changed.Fingerprint = storeHash("new-flow"), storeHash("changed-method-input-account-or-generation")
	if _, _, err := store.reserveWindowAuth(changed); err == nil {
		t.Fatal("same intent accepted changed authentication authority")
	}
	duplicate, created, err := store.reserveWindowAuth(record)
	if err != nil || created || duplicate.Flow != record.Flow {
		t.Fatal("exact intent was not reused:", created, err)
	}
	if _, err := store.SaveAccount(Account{Plugin: record.Scope.Plugin, Provider: "vendor", ID: "chosen", Auth: json.RawMessage(`{"original":true}`)}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.publishWindowAuth(t.Context(), record.Scope.Plugin, record.Flow, sdk.AuthResult{ID: "chosen", Credentials: json.RawMessage(`{"replacement":true}`)}, nil); err == nil {
		t.Fatal("new sign-in overwrote an existing account")
	}
	account, _ := store.GetAccount(record.Scope.Plugin, "vendor", "chosen")
	if account.Generation != 1 || string(account.Auth) != `{"original":true}` {
		t.Fatal("failed authentication changed existing credentials")
	}
	if err := store.DeleteAccount(record.Scope.Plugin, "vendor", "chosen", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.publishWindowAuth(t.Context(), record.Scope.Plugin, record.Flow, sdk.AuthResult{ID: "chosen", Credentials: json.RawMessage(`{"revived":true}`)}, nil); err == nil {
		t.Fatal("new sign-in revived an existing tombstone")
	}
}

func TestWindowAuthStoreReauthorizationRequiresExactGeneration(t *testing.T) {
	for _, change := range []string{"same", "stale", "foreign", "canceled"} {
		t.Run(change, func(t *testing.T) {
			store, record := authStoreFixture(t)
			account, err := store.SaveAccount(Account{Plugin: record.Scope.Plugin, Provider: "vendor", ID: "chosen", Auth: json.RawMessage(`{}`)}, 0)
			if err != nil {
				t.Fatal(err)
			}
			record.Scope.Account, record.Scope.Generation = account.ID, account.Generation
			readyAuthStore(t, store, record)
			result := sdk.AuthResult{ID: "chosen", Credentials: json.RawMessage(`{"renewed":true}`)}
			switch change {
			case "stale":
				if _, err := store.SaveAccount(account, 1); err != nil {
					t.Fatal(err)
				}
			case "foreign":
				result.ID = "foreign"
			case "canceled":
				if _, err := store.updateWindowAuth(record.Scope.Plugin, record.Flow, func(value *windowAuthRecord, _ *storeAccounts) error {
					value.Canceled = true
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			_, err = store.publishWindowAuth(t.Context(), record.Scope.Plugin, record.Flow, result, nil)
			if (err == nil) != (change == "same") {
				t.Fatalf("unexpected reauthorization result for %s: %v", change, err)
			}
		})
	}
}

func TestWindowAuthStoreUnresolvedExchangeSurvivesReopenWithoutCredentials(t *testing.T) {
	store, record := authStoreFixture(t)
	readyAuthStore(t, store, record)
	reopened := NewStore(store.root)
	pending, err := reopened.windowAuthRecord(record.Scope.Plugin, record.Flow)
	if err != nil || pending.Phase != "polling" {
		t.Fatal("lost dispatched exchange:", pending.Phase, err)
	}
	accounts, err := reopened.Accounts(record.Scope.Plugin, "vendor")
	if err != nil || len(accounts) != 0 {
		t.Fatal("unresolved exchange published an account:", err)
	}
	var private string
	err = reopened.withStore(record.Scope.Plugin, func(root *os.Root) error {
		private = filepath.Join(root.Name(), "accounts.json")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(private)
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.Replace(body, []byte(`"phase":"polling"`), []byte(`"phase":"forged"`), 1)
	if err := os.WriteFile(private, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.windowAuthRecord(record.Scope.Plugin, record.Flow); err == nil {
		t.Fatal("corrupt flow record was ignored")
	}
}

func authAccountsPath(t *testing.T, store *Store, plugin string) string {
	t.Helper()
	var path string
	if err := store.withStore(plugin, func(root *os.Root) error {
		path = filepath.Join(root.Name(), "accounts.json")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWindowAuthSchema4CorruptionDoesNotRewriteExistingBytes(t *testing.T) {
	store, record := authStoreFixture(t)
	readyAuthStore(t, store, record)
	path := authAccountsPath(t, store, record.Scope.Plugin)
	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"authFlows", "scope", "created", "expires", "interval", "continuation", "phase", "parent", "canceled"} {
		for _, missing := range []bool{false, true} {
			if field == "canceled" && missing {
				continue
			}
			t.Run(field+map[bool]string{false: "/null", true: "/missing"}[missing], func(t *testing.T) {
				var document map[string]any
				if json.Unmarshal(base, &document) != nil {
					t.Fatal("invalid fixture")
				}
				target := document
				if field != "authFlows" {
					target = document["authFlows"].([]any)[0].(map[string]any)
				}
				if missing {
					delete(target, field)
				} else {
					target[field] = nil
				}
				malformed, _ := json.Marshal(document)
				if err := os.WriteFile(path, malformed, 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Accounts(record.Scope.Plugin, "vendor"); err == nil {
					t.Fatal("corrupt schema4 was read as an empty account list")
				}
				if _, err := store.SaveAccount(Account{Plugin: record.Scope.Plugin, Provider: "vendor", ID: "new", Auth: json.RawMessage(`{}`)}, 0); err == nil {
					t.Fatal("corrupt schema4 was rewritten by SaveAccount")
				}
				if err := store.DeleteAccount(record.Scope.Plugin, "vendor", "new", 1); err == nil {
					t.Fatal("corrupt schema4 was rewritten by DeleteAccount")
				}
				after, _ := os.ReadFile(path)
				if !bytes.Equal(after, malformed) {
					t.Fatal("failed reader changed corrupt account bytes")
				}
			})
		}
	}
}

func TestWindowAuthSchema4FailedPublicationLeavesAccountAndFlowUntouched(t *testing.T) {
	store, record := authStoreFixture(t)
	readyAuthStore(t, store, record)
	large, _ := json.Marshal(map[string]string{"token": strings.Repeat("x", 3<<20)})
	if _, err := store.SaveAccount(Account{Plugin: record.Scope.Plugin, Provider: "other", ID: "retained", Auth: large}, 0); err != nil {
		t.Fatal(err)
	}
	path := authAccountsPath(t, store, record.Scope.Plugin)
	before, _ := os.ReadFile(path)
	result, _ := json.Marshal(map[string]string{"token": strings.Repeat("y", 2<<20)})
	if _, err := store.publishWindowAuth(t.Context(), record.Scope.Plugin, record.Flow, sdk.AuthResult{ID: "chosen", Credentials: result}, nil); err == nil {
		t.Fatal("oversized transaction unexpectedly committed")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed atomic publication changed account or flow bytes")
	}
	if _, err := store.GetAccount(record.Scope.Plugin, "vendor", "chosen"); err == nil {
		t.Fatal("failed receipt write published account credentials")
	}
}

func TestWindowAuthSchema4PreservesReceiptAcrossAccountOperations(t *testing.T) {
	store, record := authStoreFixture(t)
	account, err := store.SaveAccount(Account{Plugin: record.Scope.Plugin, Provider: "vendor", ID: "chosen", Auth: json.RawMessage(`{}`)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	record.Scope.Account, record.Scope.Generation = account.ID, account.Generation
	readyAuthStore(t, store, record)
	scope := record.Scope
	scope.Operation, scope.Attempt = "refresh-operation", 1
	if _, err := store.claimRefresh(scope, "revision", "0"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.publishWindowAuth(t.Context(), record.Scope.Plugin, record.Flow, sdk.AuthResult{ID: "chosen", Credentials: json.RawMessage(`{"invalid":true}`)}, nil); err == nil {
		t.Fatal("flow publication overwrote an active credential refresh")
	}
	if _, err := store.finishRefresh(scope, &sdk.AuthResult{ID: "chosen", Credentials: json.RawMessage(`{"refreshed":true}`)}, "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.publishWindowAuth(t.Context(), record.Scope.Plugin, record.Flow, sdk.AuthResult{ID: "chosen", Credentials: json.RawMessage(`{"stale":true}`)}, nil); err == nil {
		t.Fatal("flow publication adopted refresh successor generation")
	}
	if err := store.DeleteAccount(record.Scope.Plugin, "vendor", "chosen", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := store.publishWindowAuth(t.Context(), record.Scope.Plugin, record.Flow, sdk.AuthResult{ID: "chosen", Credentials: json.RawMessage(`{"revived":true}`)}, nil); err == nil {
		t.Fatal("flow publication revived a deleted selected account")
	}
	reopened := NewStore(store.root)
	after, err := reopened.windowAuthRecord(record.Scope.Plugin, record.Flow)
	if err != nil || after.Phase != "polling" || after.Scope != record.Scope {
		t.Fatal("refresh/deletion lost the broker transaction:", err)
	}
	body, _ := os.ReadFile(authAccountsPath(t, store, record.Scope.Plugin))
	if !bytes.Contains(body, []byte(`"schema":4`)) {
		t.Fatal("account mutation downgraded schema4")
	}
}

func TestWindowAuthSchema4ArchivesTerminalReceiptsWithoutReplayingIntent(t *testing.T) {
	store, first := authStoreFixture(t)
	var last windowAuthRecord
	for index := range 70 {
		record := first
		record.Flow, record.Intent = storeHash("flow", string(rune(index+1))), storeHash("intent", string(rune(index+1)))
		readyAuthStore(t, store, record)
		id := strings.Repeat("a", index+1)
		if _, err := store.publishWindowAuth(t.Context(), record.Scope.Plugin, record.Flow, sdk.AuthResult{ID: id, Credentials: json.RawMessage(`{}`)}, nil); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			first = record
		}
		last = record
	}
	old, err := NewStore(store.root).windowAuthRecord(first.Scope.Plugin, first.Flow)
	if err != nil || old.Phase != "complete" || old.Receipt == nil {
		t.Fatal("archived receipt was lost:", err)
	}
	if _, created, err := store.reserveWindowAuth(first); err != nil || created {
		t.Fatal("archived intent could exchange a code twice:", created, err)
	}
	first.Fingerprint = storeHash("changed")
	if _, _, err := store.reserveWindowAuth(first); err == nil {
		t.Fatal("archived intent accepted changed input")
	}
	if _, err := store.windowAuthRecord(last.Scope.Plugin, "../accounts"); err == nil {
		t.Fatal("flow handle became a private filesystem path")
	}
}

func TestWindowAuthSchema4ReauthorizationPreservesDirtyUsage(t *testing.T) {
	store, record := authStoreFixture(t)
	account, err := store.SaveAccount(Account{Plugin: record.Scope.Plugin, Provider: record.Scope.Provider,
		ID: "chosen", Auth: json.RawMessage(`{"token":"before"}`)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	record.Scope.Account, record.Scope.Generation = account.ID, account.Generation
	dirty, err := store.InvalidateUsage(record.Scope)
	if err != nil {
		t.Fatal(err)
	}
	readyAuthStore(t, store, record)
	if _, err := store.publishWindowAuth(t.Context(), record.Scope.Plugin, record.Flow, sdk.AuthResult{
		ID: account.ID, Credentials: json.RawMessage(`{"token":"after"}`),
	}, nil); err != nil {
		t.Fatal(err)
	}
	next := record.Scope
	next.Generation++
	stamp, err := store.UsageStamp(next)
	if err != nil || !stamp.Dirty || stamp.Revision != dirty.Revision {
		t.Fatalf("reauthorization erased pending usage authority: revision=%d dirty=%v error=%v", stamp.Revision, stamp.Dirty, err)
	}
	if err := store.UsageReady(next); err == nil {
		t.Fatal("reauthorization reopened inference before fresh usage acknowledgment")
	}
	if _, err := store.AcknowledgeUsage(next, dirty.Revision); err != nil {
		t.Fatal(err)
	}
	if err := store.UsageReady(next); err != nil {
		t.Fatal("fresh acknowledgment did not release the preserved usage barrier:", err)
	}
}

func TestWindowAuthSchema4ReauthorizationPreservesRefreshReceipts(t *testing.T) {
	store, record := authStoreFixture(t)
	account, err := store.SaveAccount(Account{Plugin: record.Scope.Plugin, Provider: record.Scope.Provider,
		ID: "chosen", Auth: json.RawMessage(`{"token":"before"}`)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: record.Scope.Plugin, Provider: record.Scope.Provider,
		Account: account.ID, Generation: account.Generation, Operation: "previous-refresh", Attempt: 1}
	if _, err := store.claimRefresh(scope, "revision", "0"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.finishRefresh(scope, &sdk.AuthResult{
		ID: account.ID, Credentials: json.RawMessage(`{"token":"refreshed"}`),
	}, "completed"); err != nil {
		t.Fatal(err)
	}
	record.Scope.Account, record.Scope.Generation = account.ID, account.Generation+1
	readyAuthStore(t, store, record)
	if _, err := store.publishWindowAuth(t.Context(), record.Scope.Plugin, record.Flow, sdk.AuthResult{
		ID: account.ID, Credentials: json.RawMessage(`{"token":"reauthorized"}`),
	}, nil); err != nil {
		t.Fatal(err)
	}
	for _, archived := range []bool{false, true} {
		if archived {
			next := record.Scope
			next.Generation, next.Operation, next.Attempt = 3, "next-refresh", 1
			if _, err := store.claimRefresh(next, "revision", "0"); err != nil {
				t.Fatal(err)
			}
		}
		receipt, err := store.Operation(scope.Plugin, scope.Operation)
		if err != nil || receipt.State != "completed" || receipt.Scope != scope {
			t.Fatalf("reauthorization erased completed refresh receipt (archived=%v): %+v, %v", archived, receipt, err)
		}
	}
}
