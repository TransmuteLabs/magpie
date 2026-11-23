package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionFreeModelKeepsModelListStates(t *testing.T) {
	host := fixtureExtensionProvider(t, false)
	const model = "fixture-free-model-state"
	host.manifest.Providers[0].Models = nil
	result := json.RawMessage(`{"models":[]}`)
	calls := 0
	host.call = func(_ context.Context, method string, _ sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if method != sdk.MethodProviderModels {
			return nil, fmt.Errorf("unexpected %s", method)
		}
		calls++
		return result, nil
	}
	p := extensionAccounts()[0]
	available := p.Available()
	_, priced := p.ListPrice(model)
	if !p.Account.Lists(model) || len(available) != 0 || priced || p.FreeModel(model) || calls != 0 {
		t.Fatalf("unfetched models = %+v, priced=%t, model RPCs=%d", available, priced, calls)
	}
	if _, err := p.Account.fetch(t.Context()); err != nil {
		t.Fatal(err)
	}
	available = p.Available()
	_, priced = p.ListPrice(model)
	if p.Account.Lists(model) || len(available) != 0 || priced || p.FreeModel(model) || calls != 1 {
		t.Fatalf("known-empty models = %+v, priced=%t, model RPCs=%d", available, priced, calls)
	}
	result = json.RawMessage(`{"models":[{"id":"fixture-free-model-state","price":{"input":7}}]}`)
	if _, err := p.Account.fetch(t.Context()); err != nil {
		t.Fatal(err)
	}
	if price, priced := p.ListPrice(model); !priced || price.Input != 7 || p.FreeModel(model) || calls != 2 {
		t.Fatalf("published price = %+v, known=%t, model RPCs=%d", price, priced, calls)
	}
}

func TestExtensionFreeModelRevalidatesSelectedAccountAndOwner(t *testing.T) {
	const model = "fixture-free-model-authority"
	for _, name := range []string{"account", "owner"} {
		t.Run(name, func(t *testing.T) {
			host := fixtureExtensionProvider(t, true)
			host.manifest.Providers[0].Models = []sdk.Model{{ID: model, Price: &sdk.ModelPrice{}}}
			providers, err := ExtensionProviders()
			if err != nil || len(providers) != 2 {
				t.Fatalf("discover SDK accounts = %d, error %v", len(providers), err)
			}
			p := providers[0]
			scope, ok := p.ExtensionScope()
			if !ok || !p.FreeModel(model) {
				t.Fatal("selected account did not expose its initial zero price")
			}
			if name == "owner" {
				usageTestSubscriptionOwner(t, host, "free-model-owner")
				owners, err := host.Store().SubscriptionOwners(t.Context())
				if err != nil || len(owners) != 1 || !owners[0].Active {
					t.Fatalf("publish current subscription owner: %d rows, error %v", len(owners), err)
				}
			} else {
				_, err := host.store.SaveAccount(extensionhost.Account{
					Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account,
					Auth: json.RawMessage(`{"access":"replacement"}`),
				}, scope.Generation)
				if err != nil {
					t.Fatal(err)
				}
			}
			if p.FreeModel(model) {
				t.Fatal("free classification reused the former SDK selection")
			}
		})
	}
}

func TestExtensionPriceAtReadsModelsOnce(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	const paid = "price-at-paid"
	const alias = "price-at-local"
	const free = "price-at-free"
	const included = "price-at-included"
	const unknown = "price-at-unknown"
	prices := map[string]float64{"alice": 4, "bob": 8}
	host.call = func(_ context.Context, method string, scope sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		price, ok := prices[scope.Account]
		if method != sdk.MethodProviderModels || !ok {
			return nil, fmt.Errorf("unexpected %s scope: %+v", method, scope)
		}
		return json.Marshal(sdk.ModelsResult{Models: []sdk.Model{
			{ID: paid, Price: &sdk.ModelPrice{Input: price, Output: 2 * price}},
			{ID: free, Price: &sdk.ModelPrice{}},
			{ID: included, Free: true, Price: &sdk.ModelPrice{Input: price}},
			{ID: unknown},
		}})
	}
	provider := extensionAccounts()[0]
	s := settings.Load()
	s.ModelIDs = map[string]string{provider.ID + "/" + alias: paid}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	accounts := append([]Provider{provider}, provider.AlsoOn()...)
	if len(accounts) != 2 {
		t.Fatalf("got %d SDK accounts, want two", len(accounts))
	}
	for _, account := range accounts {
		t.Run(account.Account.User, func(t *testing.T) {
			if _, err := account.Account.fetch(t.Context()); err != nil {
				t.Fatal(err)
			}
			reads := 0
			list := account.Account.extensionList
			account.Account.extensionList = func(ctx context.Context) []catalog.Model {
				reads++
				return list(ctx)
			}
			account.PriceRate = 0.5
			price := prices[account.Account.User] * account.PriceRate
			for _, test := range []struct {
				name, model   string
				tariff        float64
				countFree     bool
				input, output float64
				known         bool
				reads         int
			}{
				{name: "alias", model: alias, input: price, output: 2 * price, known: true, reads: 1},
				{name: "tariff", model: alias, tariff: 11, input: 11, output: 11, known: true, reads: 1},
				{name: "zero", model: free, tariff: 11, known: true, reads: 1},
				{name: "included", model: included, tariff: 11, known: true, reads: 1},
				{name: "unknown", model: unknown, reads: 1},
				{name: "count-free", model: paid, countFree: true, known: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					p := account
					p.CountFree = test.countFree
					s := settings.Settings{}
					if test.tariff != 0 {
						s.ModelPrices = map[string]settings.ModelPrice{p.ID + "/" + test.model: statePrice(test.tariff)}
					}
					reads = 0
					price, known := p.PriceAt(s, test.model, time.Time{})
					if price.Input != test.input || price.Output != test.output || known != test.known {
						t.Fatalf("PriceAt(%s) = %+v, %v; want input %g, output %g, known %v",
							test.model, price, known, test.input, test.output, test.known)
					}
					if reads != test.reads {
						t.Fatalf("PriceAt read SDK models %d times, want %d", reads, test.reads)
					}
				})
			}
		})
	}
}

func TestExtensionListPriceDoesNotCopyAccountAuth(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	const model = "fixture-list-price-auth-copy"
	const largeAuth = 32 << 10
	const maxSurcharge = 16 << 10
	host.manifest.Contract.Capabilities = []sdk.Capability{sdk.OutboundExecutor, sdk.AccountAuth}
	host.manifest.Providers[0].Models = []sdk.Model{{ID: model, Price: &sdk.ModelPrice{Input: 3, Output: 7}}}
	host.call = func(_ context.Context, method string, _ sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		t.Fatalf("listing price started SDK method %s", method)
		return nil, nil
	}

	for _, fixture := range []struct {
		id   string
		size int
	}{{"alice", 32}, {"bob", largeAuth}} {
		const prefix, suffix = `{"access":"`, `"}`
		auth := json.RawMessage(prefix + strings.Repeat("x", fixture.size-len(prefix)-len(suffix)) + suffix)
		saved, err := host.store.SaveAccount(extensionhost.Account{
			Plugin: host.manifest.ID, Provider: "vendor", ID: fixture.id, Auth: auth, ExpiresAt: 123,
		}, 1)
		if err != nil || saved.Generation != 2 || len(saved.Auth) != fixture.size {
			t.Fatalf("saved %s generation=%d auth bytes=%d, error %v", fixture.id, saved.Generation, len(saved.Auth), err)
		}
	}
	accountFile := filepath.Join(host.namespace, "private", host.manifest.ID, "accounts.json")
	info, err := os.Stat(accountFile)
	if err != nil {
		t.Fatal(err)
	}
	if size := info.Size(); size <= largeAuth || size >= 64<<10 {
		t.Fatalf("account snapshot size %d is outside the warm fixture range", size)
	}

	accounts, err := ExtensionProviders()
	if err != nil || len(accounts) != 2 {
		t.Fatalf("got %d SDK accounts, error %v", len(accounts), err)
	}
	counted := &preflightReadingHost{fixtureExtensionProviderHost: host}
	allocated := make(map[string]uint64, len(accounts))
	for _, account := range accounts {
		scope, ok := account.ExtensionScope()
		if !ok || scope.Generation != 2 || (scope.Account != "alice" && scope.Account != "bob") {
			t.Fatalf("unexpected price selection: %+v, found %t", scope, ok)
		}
		account.Account.extension.host = counted
		quote := func() {
			if price, known := account.ListPrice(model); !known || price.Input != 3 || price.Output != 7 {
				t.Fatalf("%s list price = %+v, known=%t", scope.Account, price, known)
			}
		}
		quote()
		if reads := counted.accountReads.Swap(0); reads != 1 {
			t.Fatalf("%s warm quote selected the account %d times, want 1", scope.Account, reads)
		}
		var quotes int32
		allocated[scope.Account] = leastBytes(24, func() {
			quote()
			quotes++
		})
		if reads := counted.accountReads.Swap(0); reads != quotes {
			t.Fatalf("%s prices selected the account %d times for %d quotes", scope.Account, reads, quotes)
		}
	}
	small, large := allocated["alice"], allocated["bob"]
	if small == 0 || large == 0 {
		t.Fatalf("missing allocation samples: small=%d large=%d", small, large)
	}
	after, err := os.Stat(accountFile)
	if err != nil || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		t.Fatalf("account file changed during price measurements: %v", err)
	}
	t.Logf("same-file %d bytes: auth 32 bytes -> %d B/quote, auth %d bytes -> %d B/quote", info.Size(), small, largeAuth, large)
	if large > small+maxSurcharge {
		t.Fatalf("large selected auth adds %d B/quote, want at most %d", large-small, maxSurcharge)
	}
}

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
