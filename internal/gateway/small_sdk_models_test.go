package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func smallSDKProvider(t *testing.T, minor int, models ...sdk.Model) provider.Provider {
	t.Helper()
	id := installPolicyRouteFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("candidate selection unexpectedly called the vendor")
		w.WriteHeader(http.StatusBadGateway)
	}, func(manifest *sdk.Manifest) {
		manifest.Contract = sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: minor},
			Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.AccountAuth, sdk.ModelDiscovery}}
		manifest.Providers[0].Models = models
	})
	p, err := provider.Find(id)
	if err != nil {
		t.Fatal(err)
	}
	return *p
}

func unpricedSmallSDKModels() []sdk.Model {
	models := make([]sdk.Model, 4)
	for index := range models {
		models[index] = sdk.Model{ID: fmt.Sprintf("fixture-vision-%d", index), Vision: true}
	}
	return models
}

func TestSmallCandidatesSDKPriceLookupReusesExposedModels(t *testing.T) {
	p := smallSDKProvider(t, 8, unpricedSmallSDKModels()...)
	want := len(unpricedSmallSDKModels())
	exposed := testing.AllocsPerRun(3, func() {
		if got := len(p.Exposed()); got != want {
			t.Fatalf("exposed models = %d, want %d", got, want)
		}
	})
	candidates := testing.AllocsPerRun(3, func() {
		if got := len(smallCands(p, seesAt(p.ID))); got != want {
			t.Fatalf("candidate models = %d, want %d", got, want)
		}
	})
	t.Logf("allocations per call with %d SDK vision models: exposed %.0f, candidates %.0f", want, exposed, candidates)
	if candidates > 2*exposed {
		t.Fatalf("candidate pricing allocated %.0f times; projecting the SDK models once allocated %.0f", candidates, exposed)
	}
}

func TestSmallCandidatesSDKPricesAndFreshModelList(t *testing.T) {
	const module = "fixture.codex-etag"
	var updated atomic.Bool
	id := installPolicyRouteFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		price := 2.0
		if updated.Load() {
			price = 12
		}
		models := []sdk.Model{
			{ID: "vision-paid", Vision: true, Price: &sdk.ModelPrice{Input: price, Output: 1}},
			{ID: "vision-zero", Vision: true, Price: &sdk.ModelPrice{}},
			{ID: "vision-fallback", Vision: true},
			{ID: "vision-unknown", Vision: true},
			{ID: "vision-hidden", Vision: true},
			{ID: "text-only"},
		}
		if err := json.NewEncoder(w).Encode(sdk.ModelsResult{Models: models}); err != nil {
			t.Error(err)
		}
	}, func(manifest *sdk.Manifest) {
		manifest.ID = module
		manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.ModelDiscovery)
		manifest.Providers[0].Models = nil
	})
	if err := os.MkdirAll(filepath.Dir(catalog.CachePath()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog.CachePath(), []byte(`{"openai":{"id":"openai","models":{
		"vision-zero":{"id":"vision-zero","cost":{"input":9,"output":11}},
		"vision-fallback":{"id":"vision-fallback","cost":{"input":4,"output":5}},
		"shown-fallback":{"id":"shown-fallback","cost":{"input":21,"output":22}}
	}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	catalog.Reset()
	t.Cleanup(catalog.Reset)
	policy := settings.Load()
	policy.ModelIDs = map[string]string{id + "/shown-fallback": "vision-fallback"}
	policy.ModelImages = map[string]bool{id + "/vision-hidden": false}
	if err := settings.Save(policy); err != nil {
		t.Fatal(err)
	}
	p, err := provider.Find(id)
	if err != nil {
		t.Fatal(err)
	}
	candidates := func() []smallCand { return smallCands(*p, seesAt(p.ID)) }
	if got, want := candidates(), []smallCand{{"vision-zero", 0}, {"vision-paid", 3}, {"shown-fallback", 9}, {"vision-unknown", 1e9}}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %+v, want %+v", got, want)
	}
	updated.Store(true)
	accounts, err := provider.ExtensionProviders()
	if err != nil || len(accounts) != 2 {
		t.Fatalf("SDK account count = %d, error %v", len(accounts), err)
	}
	for _, account := range accounts {
		if _, err := account.Fetch(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := candidates(), []smallCand{{"vision-zero", 0}, {"shown-fallback", 9}, {"vision-paid", 13}, {"vision-unknown", 1e9}}; !slices.Equal(got, want) {
		t.Fatalf("candidates after refresh = %+v, want %+v", got, want)
	}
	if err := extension.Default().SetEnabled(t.Context(), module, false); err != nil {
		t.Fatal(err)
	}
	if got := candidates(); len(got) != 0 {
		t.Fatalf("disabled SDK still supplied candidates: %+v", got)
	}
}
