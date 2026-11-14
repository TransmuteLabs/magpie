package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func normalizedFact[Value any](value Value) *Value { return &value }

func TestNormalizedExecutionUsageKeepsSparseEvidence(t *testing.T) {
	raw := Usage{Input: 90, Output: 20, CacheRead: 30, CacheWrite: 15, CacheWrite1h: 6,
		Reasoning: 12, Served: "raw-model", Tier: "priority", RequestID: "raw-request",
		ResponseID: "client-response",
		Deferred:   &Usage{Input: 4, Output: 3}}
	totals := &sdk.Usage{Facts: &sdk.ExecutionUsageFacts{
		ExecutionCounts: sdk.ExecutionCounts{InputTokens: normalizedFact(int64(0)), CacheWriteTokens: normalizedFact(int64(2))},
		Tier:            normalizedFact(""), Credits: normalizedFact(0.0),
		Deferred:   &sdk.ExecutionCounts{OutputTokens: normalizedFact(int64(0))},
		ResponseID: normalizedFact("upstream-response"),
	}}
	merged := normalizedExtensionTotals(totals, raw)
	if merged.Input != 0 || merged.Output != 20 || merged.CacheRead != 30 || merged.CacheWrite != 2 ||
		merged.CacheWrite1h != 0 || merged.Reasoning != 12 || merged.Served != "raw-model" ||
		merged.RequestID != "raw-request" || merged.ResponseID != "client-response" ||
		merged.UpstreamResponseID != "upstream-response" || merged.Tier != "" || merged.Credits == nil || *merged.Credits != 0 ||
		merged.Deferred == nil || merged.Deferred.Input != 4 || merged.Deferred.Output != 0 {
		t.Fatalf("sparse normalized usage erased raw evidence or ignored measured zero: %+v", merged)
	}
	if raw.Input != 90 || raw.Deferred.Output != 3 {
		t.Fatal("normalization mutated the raw snapshot")
	}
	legacy := normalizedExtensionTotals(&sdk.Usage{}, raw)
	if legacy.Input != 0 || legacy.Output != 0 || legacy.CacheRead != 0 || legacy.CacheWrite != 0 ||
		legacy.CacheWrite1h != 0 || legacy.RequestID != raw.RequestID || legacy.Reasoning != raw.Reasoning {
		t.Fatalf("legacy whole-count snapshots changed semantics: %+v", legacy)
	}
}

func TestNormalizedExecutionUsageReachesAllMediaAccountingOwners(t *testing.T) {
	_, _, id := installExtensionMediaFixture(t)
	server := New()
	t.Cleanup(server.idle.stop)
	for _, sample := range []struct{ path, model, field string }{
		{"/v1/embeddings", "fixture-vector", `"input":"rich normalized usage"`},
		{"/v1/rerank", "fixture-ranker", `"query":"rich normalized usage","documents":["fixture"]`},
		{"/v1/audio/speech", "fixture-voice", `"input":"rich normalized usage","voice":"alloy"`},
		{"/v1/images/generations", "fixture-picture", `"prompt":"rich normalized usage"`},
		{"/v1/videos", "fixture-movie", `"prompt":"rich normalized usage"`},
	} {
		t.Run(sample.model, func(t *testing.T) {
			response := mediaFixtureCall(server.Front(), http.MethodPost, sample.path,
				`{"model":"`+id+`/`+sample.model+`",`+sample.field+`}`)
			if response.Code != http.StatusOK {
				t.Fatalf("rich accounting operation failed: %d %s", response.Code, response.Body)
			}
			var records []usage.Record
			for _, record := range usage.Load(time.Time{}) {
				if record.Model == sample.model {
					records = append(records, record)
				}
			}
			if len(records) != 1 {
				t.Fatalf("one execution became %d accounting rows", len(records))
			}
			record := records[0]
			if record.Input != 28 || record.Output != 6 || record.CacheRead != 4 || record.CacheWrite != 7 ||
				record.CacheWrite1h != 3 || record.Reasoning != 2 || record.TierServed != "flex" ||
				record.Served != "served-fixture" || record.Upstream != "fixture-upstream" ||
				record.RequestID != "fixture-request" || record.UpstreamResponseID != "fixture-response" || record.Stop != "completed" ||
				record.Credits == nil || *record.Credits != 0.25 || record.ReportedCost == nil ||
				record.ReportedCost.Currency != "USD" || record.OwnCost == nil || *record.OwnCost != 0.002 {
				t.Fatalf("ordinary media ledger lost normalized accounting evidence: %+v", record)
			}
			calls := server.Recent()
			if len(calls) == 0 || calls[0].Usage.Deferred == nil || calls[0].Usage.ReportedCost == nil ||
				calls[0].Usage.ReportedCost.Currency != "USD" || calls[0].Usage.CacheWrite1h != 3 {
				t.Fatalf("ordinary media Recent lost normalized evidence: %+v", calls)
			}
			matched := false
			for _, route := range server.Trace(t.Context(), 0, 0).Routes {
				if route.ID != record.RouteID {
					continue
				}
				matched = true
				if len(route.Usage) != 2 || route.Usage[0].Input != 11 || route.Usage[1].Input != 17 ||
					route.Usage[1].CacheWrite1h != 3 || route.Usage[1].ReportedCost == nil ||
					route.Usage[1].PricingRecord().CostAt(catalog.Price{Input: 50}) != 0.002 {
					t.Fatalf("ordinary routing history lost deferred or monetary evidence: %+v", route.Usage)
				}
			}
			if !matched {
				t.Fatal("normalized accounting has no ordinary route")
			}
		})
	}
}

func TestNormalizedExecutionUsageDoesNotInventCurrencyConversion(t *testing.T) {
	totals := normalizedExtensionTotals(&sdk.Usage{Facts: &sdk.ExecutionUsageFacts{
		ExecutionCounts: sdk.ExecutionCounts{InputTokens: normalizedFact(int64(1000))},
		Cost:            &sdk.ExecutionCost{Amount: 42, Currency: "EUR"},
	}}, Usage{})
	record := accountedUsage(usage.Record{}, totals)
	if record.OwnCost != nil || record.ReportedCost == nil || record.ReportedCost.Amount != 42 ||
		record.ReportedCost.Currency != "EUR" || record.CostAt(catalog.Price{Input: 2}) == 42 {
		t.Fatalf("a reported non-USD amount acquired an invented USD value: %+v", record)
	}
	routes := routeUsage("vendor", "model", totals)
	if len(routes) != 1 || routes[0].PricingRecord().OwnCost != nil ||
		routes[0].ReportedCost.Currency != "EUR" {
		t.Fatalf("routing history lost a non-USD reported amount: %+v", routes)
	}
	zero := 0.0
	if routes := routeUsage("vendor", "model", Usage{Credits: &zero}); len(routes) != 1 {
		t.Fatal("a measured zero credit amount disappeared with absent token counts")
	}
}
