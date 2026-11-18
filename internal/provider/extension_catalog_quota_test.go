package provider

import (
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionCatalogProjectionQuotaFollowsRenamedModels(t *testing.T) {
	providerFileHome(t)
	configured := Provider{ID: "quota-alias", Name: "Quota alias", Chat: "http://127.0.0.1:1/v1",
		Key: "synthetic-key", Models: []string{"m1"}}
	if err := Save(configured); err != nil {
		t.Fatal(err)
	}
	readAt := time.Now()
	keepLast(SubscriptionQuota{Provider: configured.ID, ReadAt: &readAt,
		Windows: []QuotaWindow{{Name: "model-quota", Model: "m1", Used: 90}}},
		keyTag("plan", configured.Key))
	check := func(t *testing.T, want string) {
		t.Helper()
		result, err := extensionCatalogProjection(t.Context(), sdk.Scope{Plugin: "reader"},
			sdk.CatalogAccessDescriptor{Scope: "inventory"})
		if err != nil || len(result.Accounts) != 1 || len(result.Accounts[0].Models) != 1 ||
			result.Accounts[0].Models[0].ID != want {
			t.Fatal("ordinary visible model projection failed", result, err)
		}
		quota := result.Accounts[0].Quota
		if !quota.Known || len(quota.Windows) != 1 || len(quota.Windows[0].Models) != 1 ||
			quota.Windows[0].Models[0] != want || quota.Windows[0].Used == nil || *quota.Windows[0].Used != 90 {
			t.Fatalf("cached quota lost its visible model mapping: model=%s quota=%+v", want, quota)
		}
	}
	t.Run("original", func(t *testing.T) { check(t, "quota-alias/m1") })
	if _, changed, err := SetModelID("quota-alias/m1", "friendly"); err != nil || !changed {
		t.Fatal("ordinary model alias failed", changed, err)
	}
	t.Run("renamed", func(t *testing.T) { check(t, "quota-alias/friendly") })
}

func TestExtensionCatalogProjectionReadsCachedBalanceWindows(t *testing.T) {
	keyLimitsHome(t)
	week := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	server, asked := sub2apiServer(t, func() ([]byte, int) { return weekUsed(t, "720", week), 200 })
	configured := sub2apiKey(t, server)
	configured.Models = []string{"m1"}
	if err := Save(configured); err != nil {
		t.Fatal(err)
	}
	waitAllowance(t, configured, func(allowance Allowance) bool { return len(allowance) == 2 })
	card := cardOf(CachedCards(time.Now()), configured.ID, "")
	if card == nil || card.Kind != "balance" || len(card.Windows) != 2 || card.Windows[1].Used != 90 {
		t.Fatal("ordinary cached balance card lacks its measured quota", card)
	}
	before := asked.Load()
	result, err := extensionCatalogProjection(t.Context(), sdk.Scope{Plugin: "reader"},
		sdk.CatalogAccessDescriptor{Scope: "inventory"})
	if asked.Load() != before {
		t.Fatal("catalog projection refreshed the vendor")
	}
	if err != nil || len(result.Accounts) != 1 || len(result.Accounts[0].Models) != 1 {
		t.Fatal("ordinary key projection failed", result, err)
	}
	quota := result.Accounts[0].Quota
	if !quota.Known || len(quota.Windows) != 2 || quota.Windows[1].Used == nil || *quota.Windows[1].Used != 90 {
		t.Fatalf("cached balance quota disappeared: ordinary=%+v projection=%+v", card.Windows, quota)
	}
}

func TestExtensionCatalogProjectionKeepsBothKeyCardKinds(t *testing.T) {
	providerFileHome(t)
	configured := Provider{ID: "quota-kinds", Name: "Quota kinds", Chat: "http://127.0.0.1:1/v1",
		Key: "synthetic-key", Models: []string{"m1"}}
	if err := Save(configured); err != nil {
		t.Fatal(err)
	}
	readAt := time.Now()
	for _, kind := range []string{"plan", "balance"} {
		keepLast(SubscriptionQuota{Provider: configured.ID, ReadAt: &readAt,
			Windows: []QuotaWindow{{Name: kind, Used: 90}}}, keyTag(kind, configured.Key))
	}
	result, err := extensionCatalogProjection(t.Context(), sdk.Scope{Plugin: "reader"},
		sdk.CatalogAccessDescriptor{Scope: "inventory"})
	if err != nil || len(result.Accounts) != 1 {
		t.Fatal("ordinary key projection failed", result, err)
	}
	quota := result.Accounts[0].Quota
	if !quota.Known || len(quota.Windows) != 2 || quota.Windows[0].Name != "plan" || quota.Windows[1].Name != "balance" {
		t.Fatalf("catalog dropped one cached key-card kind: %+v", quota)
	}
}
