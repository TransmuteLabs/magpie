package extension

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

func modelPolicyManifest() Manifest {
	manifest := testManifest(OutboundExecutor)
	manifest.Providers = []ProviderDescriptor{providerDescriptor()}
	return manifest
}

func TestModelPriceAndEffortPolicyValidation(t *testing.T) {
	manifest := modelPolicyManifest()
	model := &manifest.Providers[0].Models[0]
	model.Price = &ModelPrice{Currency: "USD", Input: 3, Output: 5, CacheRead: 0.5, CacheWrite: 1, CacheWrite1h: 2}
	model.Efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
	if err := manifest.Validate(); err != nil {
		t.Fatal("canonical model policy was refused", err)
	}
	for _, bad := range []string{"auto", "ultra", "light", "extra_high", "High", "", "unknown"} {
		candidate := modelPolicyManifest()
		candidate.Providers[0].Models[0].Efforts = []string{bad}
		if err := candidate.Validate(); err == nil {
			t.Fatal("unknown effort was advertised", bad)
		}
	}
	for _, efforts := range [][]string{{"low", "low"}, {"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "low"}} {
		candidate := modelPolicyManifest()
		candidate.Providers[0].Models[0].Efforts = efforts
		if err := candidate.Validate(); err == nil {
			t.Fatal("duplicate or excessive effort list was accepted", efforts)
		}
	}
	for _, currency := range []string{"EUR", "usd", "USD ", "unknown"} {
		candidate := modelPolicyManifest()
		candidate.Providers[0].Models[0].Price = &ModelPrice{Currency: currency}
		if err := candidate.Validate(); err == nil {
			t.Fatal("non-USD price was accepted", currency)
		}
	}
	for _, field := range []string{"Input", "Output", "CacheRead", "CacheWrite", "CacheWrite1h"} {
		t.Run(field, func(t *testing.T) {
			for _, value := range []float64{-0.01, math.NaN(), math.Inf(1), math.Inf(-1)} {
				candidate := modelPolicyManifest()
				price := &ModelPrice{}
				reflect.ValueOf(price).Elem().FieldByName(field).SetFloat(value)
				candidate.Providers[0].Models[0].Price = price
				if err := candidate.Validate(); err == nil {
					t.Fatalf("invalid %s rate was accepted: %v", field, value)
				}
			}
		})
	}
}

func TestModelPolicyPreservesUnknownVersusExplicitlyFreePrice(t *testing.T) {
	for _, price := range []*ModelPrice{nil, {}, {Currency: "USD"}} {
		manifest := modelPolicyManifest()
		manifest.Providers[0].Models[0].Price = price
		if err := manifest.Validate(); err != nil {
			t.Fatal("unknown or zero price was refused", err)
		}
		body, err := json.Marshal(manifest.Providers[0].Models[0])
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]json.RawMessage
		var model Model
		if json.Unmarshal(body, &wire) != nil || json.Unmarshal(body, &model) != nil {
			t.Fatal("model price did not roundtrip")
		}
		if price == nil {
			if wire["price"] != nil || model.Price != nil {
				t.Fatal("unknown price became a free price", string(body))
			}
		} else if wire["price"] == nil || model.Price == nil || *model.Price != *price {
			t.Fatal("explicit free price became unknown", string(body))
		}
	}
}

func TestModelPolicyPublicAndWorkerSnapshotsDeepCopyPriceAndEfforts(t *testing.T) {
	manifest := modelPolicyManifest()
	manifest.Providers[0].Models[0].Price = &ModelPrice{Input: 3, Output: 5, CacheWrite1h: 2}
	manifest.Providers[0].Models[0].Efforts = []string{"low", "high"}
	module := Module{Manifest: manifest, Handlers: map[string]Handler{MethodProviderExecute: testNoop}}
	handlers, _, err := module.prepare()
	if err != nil {
		t.Fatal(err)
	}
	public := manifest.Public()
	manifest.Providers[0].Models[0].Price.Input = 99
	manifest.Providers[0].Models[0].Efforts[0] = "none"
	model := public.Providers[0].Models[0]
	if model.Price.Input != 3 || model.Efforts[0] != "low" {
		t.Fatal("public snapshot aliases model policy")
	}
	model.Price.Output = 77
	model.Efforts[1] = "ultra"
	if manifest.Providers[0].Models[0].Price.Output != 5 || manifest.Providers[0].Models[0].Efforts[1] != "high" {
		t.Fatal("public snapshot can mutate original model policy")
	}
	first, err := handlers[MethodDescribe](context.Background(), Call{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	frozen := first.(Manifest).Providers[0].Models[0]
	if frozen.Price.Input != 3 || frozen.Price.Output != 5 || !reflect.DeepEqual(frozen.Efforts, []string{"low", "high"}) {
		t.Fatal("worker handshake aliases mutable model policy")
	}
	body, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Price struct {
			CacheWrite1h float64 `json:"cacheWrite1h"`
		} `json:"price"`
	}
	if json.Unmarshal(body, &wire) != nil || wire.Price.CacheWrite1h != 2 {
		t.Fatal("price cache TTL rate lost its wire name", string(body))
	}
}
