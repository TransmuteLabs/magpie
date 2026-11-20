package extension

import (
	"encoding/json"
	"strconv"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestWindowAuthStoreReaddsForgottenAccount(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		existingUI bool
	}{
		{name: "legacy-store"},
		{name: "broker-store", existingUI: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			store, fresh := authStoreFixture(t)
			plugin, provider := fresh.Scope.Plugin, fresh.Scope.Provider
			first, err := store.SaveAccount(Account{
				Plugin: plugin, Provider: provider, ID: "public", Auth: json.RawMessage(`{"token":"first"}`),
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.existingUI {
				older := fresh
				older.Flow, older.Intent = storeHash("older-flow"), storeHash("older-intent")
				readyAuthStore(t, store, older)
			}
			if err := store.DeleteAccount(plugin, provider, first.ID, first.Generation); err != nil {
				t.Fatal(err)
			}
			reopened := NewStore(store.root)
			readyAuthStore(t, reopened, fresh)
			receipt, err := reopened.publishWindowAuth(plugin, fresh.Flow, sdk.AuthResult{
				ID: first.ID, Credentials: json.RawMessage(`{"token":"second"}`),
			})
			generation := first.Generation + 2
			if err != nil || receipt.Receipt == nil || receipt.Receipt.Generation != strconv.FormatUint(generation, 10) {
				t.Fatalf("fresh sign-in could not re-add a forgotten account at generation %d: %v", generation, err)
			}
			duplicate := fresh
			duplicate.Flow, duplicate.Intent = storeHash("duplicate-flow"), storeHash("duplicate-intent")
			readyAuthStore(t, reopened, duplicate)
			if _, err := reopened.publishWindowAuth(plugin, duplicate.Flow, sdk.AuthResult{
				ID: first.ID, Credentials: json.RawMessage(`{"token":"replacement"}`),
			}); err == nil {
				t.Fatal("unselected sign-in replaced an active account")
			}
			account, err := store.GetAccount(plugin, provider, first.ID)
			if err != nil || account.Generation != generation || string(account.Auth) != `{"token":"second"}` {
				t.Fatalf("re-added account lost its credential or generation: %d, %v", account.Generation, err)
			}
		})
	}
}

func TestWindowAuthStoreForgetFencesEarlierUnselectedSignIns(t *testing.T) {
	store, initial := authStoreFixture(t)
	plugin, provider := initial.Scope.Plugin, initial.Scope.Provider
	account, err := store.SaveAccount(Account{
		Plugin: plugin, Provider: provider, ID: "forgotten", Auth: json.RawMessage(`{}`),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.SaveAccount(Account{
		Plugin: plugin, Provider: provider, ID: "retained", Auth: json.RawMessage(`{}`),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	selected := initial
	selected.Flow, selected.Intent = storeHash("selected-flow"), storeHash("selected-intent")
	selected.Scope.Account, selected.Scope.Generation = other.ID, other.Generation
	readyAuthStore(t, store, selected)
	foreign := initial
	foreign.Flow, foreign.Intent = storeHash("foreign-flow"), storeHash("foreign-intent")
	foreign.Scope.Provider = "foreign-provider"
	readyAuthStore(t, store, foreign)
	before := []windowAuthRecord{}
	for _, phase := range []string{"reserved", "starting", "waiting", "polling"} {
		record := initial
		record.Flow, record.Intent = storeHash(phase, "flow"), storeHash(phase, "intent")
		if _, created, err := store.reserveWindowAuth(record); err != nil || !created {
			t.Fatalf("%s reserve: %v", phase, err)
		}
		record, err = store.updateWindowAuth(plugin, record.Flow, func(value *windowAuthRecord, _ *storeAccounts) error {
			value.Phase = phase
			if phase == "waiting" || phase == "polling" {
				value.Interval, value.Next, value.Continuation = 1000, 1, json.RawMessage(`{}`)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		before = append(before, record)
	}
	reopened := NewStore(store.root)
	if err := reopened.DeleteAccount(plugin, provider, account.ID, account.Generation); err != nil {
		t.Fatal(err)
	}
	for _, record := range before {
		after, err := store.windowAuthRecord(plugin, record.Flow)
		if err != nil || !after.Canceled || after.public().State != "canceled" {
			t.Errorf("%s sign-in did not lose publication authority: %s, %v", record.Phase, after.public().State, err)
		}
		if _, err := store.publishWindowAuth(plugin, record.Flow, sdk.AuthResult{ID: account.ID, Credentials: json.RawMessage(`{}`)}); err == nil {
			t.Errorf("%s sign-in restored the forgotten account", record.Phase)
		}
	}
	for _, record := range []windowAuthRecord{selected, foreign} {
		id := record.Scope.Account
		if id == "" {
			id = account.ID
		}
		if _, err := store.publishWindowAuth(plugin, record.Flow, sdk.AuthResult{ID: id, Credentials: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("forget blocked an independently scoped sign-in for %s: %v", record.Scope.Provider, err)
		}
	}
}
