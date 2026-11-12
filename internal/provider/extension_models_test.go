package provider

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionProviderCustomModelPricesAndEffortsReachCorePolicy(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	var models []sdk.Model
	if err := json.Unmarshal([]byte(`[
		{"id":"fixture-sdk-priced","protocols":["chat"],"price":{"currency":"USD","input":3,"output":5,"cacheRead":0.5,"cacheWrite":1,"cacheWrite1h":2.5},"efforts":["low","high"]},
		{"id":"fixture-sdk-free","protocols":["chat"],"price":{}},
		{"id":"fixture-sdk-unpriced","protocols":["chat"]}
	]`), &models); err != nil {
		t.Fatal(err)
	}
	host.manifest.Providers[0].Models = models
	provider := extensionAccounts()[0]
	priced, ok := provider.ListPrice("fixture-sdk-priced")
	if !ok || priced.Input != 3 || priced.Output != 5 || priced.CacheRead != 0.5 || priced.CacheWrite != 1 || priced.CacheWrite1h != 2.5 {
		t.Fatalf("custom SDK model lost its USD/million price: %+v known=%v", priced, ok)
	}
	if got := provider.Efforts("fixture-sdk-priced"); !slices.Equal(got, []string{"low", "high"}) ||
		!provider.Thinks("fixture-sdk-priced") {
		t.Fatalf("custom SDK model lost its explicit reasoning levels: %v", got)
	}
	s := settings.Load()
	if _, ok := provider.ListPrice("fixture-sdk-unpriced"); ok || provider.PricedFree(s, "fixture-sdk-unpriced") {
		t.Fatal("an absent SDK price became a known free price")
	}
	if free, ok := provider.ListPrice("fixture-sdk-free"); !ok || !zeroPrice(free) ||
		!provider.PricedFree(s, "fixture-sdk-free") {
		t.Fatalf("an explicit free SDK price was discarded: %+v known=%v", free, ok)
	}
	provider.PriceRate = 0.5
	if counted, ok := provider.PriceAt(s, "fixture-sdk-priced", time.Time{}); !ok ||
		counted.Input != 1.5 || counted.Output != 2.5 {
		t.Fatalf("SDK price bypassed the core price rate: %+v known=%v", counted, ok)
	}
	s.ModelPrices = map[string]settings.ModelPrice{provider.ID + "/fixture-sdk-priced": statePrice(11)}
	if counted, ok := provider.PriceAt(s, "fixture-sdk-priced", time.Time{}); !ok ||
		counted.Input != 11 || counted.Output != 11 {
		t.Fatalf("SDK price displaced the user's core price: %+v known=%v", counted, ok)
	}
	host.call = func(_ context.Context, method string, _ sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if method != sdk.MethodProviderModels {
			t.Fatalf("unexpected model refresh method: %s", method)
		}
		return json.RawMessage(`{"models":[{"id":"fixture-sdk-priced","protocols":["chat"],
			"price":{"input":8,"output":13,"cacheRead":2,"cacheWrite":4,"cacheWrite1h":10},"efforts":["medium"]}]}`), nil
	}
	if _, err := provider.Account.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if current, ok := provider.ListPrice("fixture-sdk-priced"); !ok || current.Input != 8 || current.Output != 13 ||
		current.CacheRead != 2 || current.CacheWrite != 4 || current.CacheWrite1h != 10 {
		t.Fatalf("refreshed SDK model price was lost: %+v known=%v", current, ok)
	}
	if got := provider.Efforts("fixture-sdk-priced"); !slices.Equal(got, []string{"medium"}) {
		t.Fatalf("refreshed SDK reasoning levels were lost: %v", got)
	}
}

func TestExtensionProviderInvalidModelRefreshKeepsValidatedSnapshot(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	provider := extensionAccounts()[0]
	raw := json.RawMessage(`{"models":[{"id":"fixture-sdk-validated","price":{"input":2,"output":7},"efforts":["high"]}]}`)
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		return raw, nil
	}
	if _, err := provider.Account.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`"price":{"input":-1}`,
		`"price":{"cacheWrite1h":-1}`,
		`"price":{"input":1e309}`,
		`"price":{"currency":"EUR","input":1}`,
		`"efforts":["ultra"]`,
		`"efforts":["high","high"]`,
	} {
		raw = json.RawMessage(`{"models":[{"id":"fixture-sdk-validated",` + bad + `}]}`)
		if _, err := provider.Account.fetch(context.Background()); err == nil {
			t.Fatalf("invalid model metadata was accepted: %s", bad)
		}
		price, ok := provider.ListPrice("fixture-sdk-validated")
		if !ok || price.Input != 2 || price.Output != 7 || !slices.Equal(provider.Efforts("fixture-sdk-validated"), []string{"high"}) {
			t.Fatalf("invalid refresh replaced the validated snapshot: %s price=%+v known=%v", bad, price, ok)
		}
	}
}
