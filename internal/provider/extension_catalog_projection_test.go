package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionCatalogProjectionUsesOrdinaryKeyModelAndVisibilityPolicy(t *testing.T) {
	providerFileHome(t)
	configured := Provider{ID: "catalog", Name: "Catalog", Chat: "http://127.0.0.1:1/v1",
		Key: "private-first-token", Keys: []KeyAccount{{Key: "private-second-token"}, {Key: "private-disabled-token", Off: true}},
		Models: []string{"m1", "m2"}, AccountModels: map[string][]string{
			KeyID("private-first-token"): {"m1"}, KeyID("private-second-token"): {"m2"},
		}}
	if err := Save(configured); err != nil {
		t.Fatal(err)
	}
	if err := Save(Provider{ID: "hidden", Name: "Hidden", Chat: "http://127.0.0.1:1/v1",
		Key: "hidden-token", Models: []string{"m1"}, Unlisted: true}); err != nil {
		t.Fatal(err)
	}
	result, err := extensionCatalogProjection(t.Context(), sdk.Scope{Plugin: "reader"},
		sdk.CatalogAccessDescriptor{Scope: "inventory"})
	if err != nil || len(result.Accounts) != 2 {
		t.Fatal("ordinary catalog/key visibility did not reach the projection", result, err)
	}
	for index, account := range result.Accounts {
		if account.Provider != "catalog" || account.Kind != "key" || len(account.Models) != 1 ||
			account.Models[0].ID != "catalog/m"+strconv.Itoa(index+1) || account.Quota.Known {
			t.Fatal("projection ignored account restrictions or invented a quota", result)
		}
	}
	encoded, _ := json.Marshal(result)
	for _, private := range []string{"private-", "hidden-token", "127.0.0.1", "Authorization", `"key"` + ":"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("catalog projection exposed private provider configuration", string(encoded))
		}
	}
	if err := SetAccountModels("catalog", KeyID("private-first-token"), []string{"m2"}); err != nil {
		t.Fatal(err)
	}
	updated, err := extensionCatalogProjection(t.Context(), sdk.Scope{Plugin: "reader"},
		sdk.CatalogAccessDescriptor{Scope: "inventory"})
	if err != nil || len(updated.Accounts) != 2 || updated.Accounts[0].Models[0].ID != "catalog/m2" ||
		updated.Accounts[0].Ref != result.Accounts[0].Ref {
		t.Fatal("ordinary account policy changed without changing its projection", updated, err)
	}
}

func TestExtensionCatalogProjectionKeepsAccountSelectionAndNeverFetchesVendor(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	host.manifest.Providers[0].Models = append(host.manifest.Providers[0].Models, sdk.Model{ID: "m2", ContextWindow: 256000})
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		t.Error("catalog projection asked a vendor instead of its saved catalog")
		return nil, nil
	}
	providers := extensionAccounts()
	if len(providers) != 1 {
		t.Fatal("fixture did not create its ordinary provider")
	}
	id := providers[0].ID
	if err := SetAccountModels(id, "alice", []string{"m1"}); err != nil {
		t.Fatal(err)
	}
	if err := SetAccountModels(id, "bob", []string{"m2"}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"alice", "bob"} {
		scope := sdk.Scope{Plugin: host.manifest.ID, Provider: "vendor", Account: account, Generation: 1}
		result, err := extensionCatalogProjection(t.Context(), scope, sdk.CatalogAccessDescriptor{Scope: "selected"})
		want := "m1"
		if account == "bob" {
			want = "m2"
		}
		if err != nil || len(result.Accounts) != 1 || len(result.Accounts[0].Models) != 1 ||
			result.Accounts[0].Models[0].ID != id+"/"+want {
			t.Fatal("selected catalog account used another account's models", result, err)
		}
	}
}

func TestExtensionCatalogProjectionPreservesQuotaApplicabilityAndUnknown(t *testing.T) {
	readAt, reset := time.Unix(100, 0), time.Unix(200, 0)
	models := []sdk.CatalogModelProjection{{ID: "vendor/opus"}, {ID: "vendor/sonnet"}}
	projected := projectedCatalogQuota(Provider{}, SubscriptionQuota{ReadAt: &readAt, Windows: []QuotaWindow{
		{Name: "shared", Used: 0, ResetsAt: &reset},
		{Name: "opus", Used: 90, Model: "opus"},
		{Name: "unknown", Unknown: true},
		{Name: "extra", Aside: true, Credits: true, Unlimited: true},
	}}, models)
	if !projected.Known || projected.AsOf != readAt.UnixMilli() || len(projected.Windows) != 4 ||
		projected.Windows[0].Used == nil || *projected.Windows[0].Used != 0 ||
		projected.Windows[0].ResetsAt != reset.UnixMilli() || len(projected.Windows[0].Models) != 2 ||
		len(projected.Windows[1].Models) != 1 || projected.Windows[1].Models[0] != "vendor/opus" ||
		projected.Windows[2].Used != nil || !projected.Windows[3].Aside || !projected.Windows[3].Credits || !projected.Windows[3].Unlimited {
		t.Fatal("quota projection changed measured zero, applicability or unknown evidence", projected)
	}
	if projected := projectedCatalogQuota(Provider{}, SubscriptionQuota{Error: "private vendor failure",
		Windows: []QuotaWindow{{Name: "daily", Used: 0}}}, models); projected.Known || len(projected.Windows) != 0 {
		t.Fatal("a failed quota read became an empty allowance", projected)
	}
}

func TestExtensionCatalogProjectionQuotaProofHonorsDeadline(t *testing.T) {
	old, _, _ := prepareUsageReadFixture(t)
	id := ExtensionProviderID(old.manifest.ID, "vendor")
	quota := usageTestRead(t, id, "alice", 20)
	current := fixtureExtensionProvider(t, true)
	warmSubscriptionUsageForTest(t, quota)
	scope := sdk.Scope{Plugin: current.manifest.ID, Provider: "vendor", Account: "alice", Generation: 1}
	access := sdk.CatalogAccessDescriptor{Scope: "selected"}
	if result, err := extensionCatalogProjection(t.Context(), scope, access); err != nil ||
		len(result.Accounts) != 1 || !result.Accounts[0].Quota.Known {
		t.Fatalf("fixture did not reach its warm quota projection: %+v %v", result, err)
	}
	unlock, held := filelock.Lock(filepath.Join(old.Namespace(), "private", ".lock"), time.Second)
	if !held {
		t.Fatal("could not hold the quota proof Store lock")
	}
	synctest.Test(t, func(t *testing.T) {
		type projection struct {
			result sdk.CatalogReadResult
			err    error
		}
		done := make(chan projection, 1)
		go func() {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			result, err := extensionCatalogProjection(ctx, scope, access)
			done <- projection{result, err}
		}()
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case got := <-done:
			unlock()
			if got.err == nil || len(got.result.Accounts) != 0 {
				t.Errorf("a timed-out projection published quota: %+v %v", got.result, got.err)
			}
		default:
			unlock()
			<-done
			t.Error("projection remained blocked by the cached proof after its deadline")
		}
	})
}

func TestExtensionCatalogProjectionColdStoreHonorsDeadline(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	scope := sdk.Scope{Plugin: host.manifest.ID, Provider: "vendor", Account: "alice", Generation: 1}
	access := sdk.CatalogAccessDescriptor{Scope: "selected"}
	Changed()
	unlock, held := filelock.Lock(filepath.Join(host.Namespace(), "private", ".lock"), time.Second)
	if !held {
		t.Fatal("could not hold the current SDK Store")
	}
	synctest.Test(t, func(t *testing.T) {
		done := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			result, err := extensionCatalogProjection(ctx, scope, access)
			if len(result.Accounts) != 0 || result.ReadAt != 0 {
				t.Errorf("a canceled read published a catalog: %+v", result)
			}
			done <- err
		}()
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case err := <-done:
			unlock()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("cold catalog error = %v, want deadline exceeded", err)
			}
		default:
			unlock()
			<-done
			t.Error("cold catalog discovery still held the request after its deadline")
		}
	})
	result, err := extensionCatalogProjection(t.Context(), scope, access)
	if err != nil || len(result.Accounts) != 1 || len(result.Accounts[0].Models) != 1 ||
		result.Accounts[0].Models[0].ID != ExtensionProviderID(scope.Plugin, scope.Provider)+"/m1" {
		t.Fatalf("canceled discovery spoiled the next selected catalog: %+v %v", result, err)
	}
	scope.Generation++
	if result, err := extensionCatalogProjection(t.Context(), scope, access); err == nil || len(result.Accounts) != 0 {
		t.Fatalf("selected catalog accepted a different generation: %+v %v", result, err)
	}
}

func TestExtensionCatalogProjectionRefusesUnreadableProviderSource(t *testing.T) {
	providerFileHome(t)
	if err := os.MkdirAll(Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := extensionCatalogProjection(t.Context(), sdk.Scope{Plugin: "reader"},
		sdk.CatalogAccessDescriptor{Scope: "inventory"})
	if err == nil || result.ReadAt != 0 || len(result.Accounts) != 0 {
		t.Fatal("an unreadable provider source became an empty catalog", result, err)
	}
}
