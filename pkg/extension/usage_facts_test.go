package extension

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func executionFact[Value any](value Value) *Value { return &value }

func TestExecutionUsageFactsPreserveSparseZeroAndCloneOwnership(t *testing.T) {
	totals := &Usage{Facts: &ExecutionUsageFacts{
		ExecutionCounts: ExecutionCounts{InputTokens: executionFact(int64(0)), CacheWriteTokens: executionFact(int64(7)),
			CacheWrite1hTokens: executionFact(int64(3)), ReasoningTokens: executionFact(int64(0))},
		CacheUnknown: executionFact(false), Tier: executionFact(""),
		Deferred: &ExecutionCounts{InputTokens: executionFact(int64(12))},
		Credits:  executionFact(0.0), Cost: &ExecutionCost{Amount: 0, Currency: "USD"},
	}}
	if err := ValidateExecutionUsage(Manifest{Contract: FullContract()}, totals); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(totals)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"inputTokens":0`, `"reasoningTokens":0`, `"cacheUnknown":false`, `"tier":""`, `"credits":0`, `"amount":0`} {
		if !strings.Contains(string(raw), required) {
			t.Fatalf("present zero lost its wire evidence: %s lacks %s", raw, required)
		}
	}
	if strings.Contains(string(raw), `"outputTokens"`) {
		t.Fatalf("absent token count became measured zero: %s", raw)
	}
	copy := totals.Clone()
	*totals.Facts.InputTokens = 99
	*totals.Facts.CacheWrite1hTokens = 7
	*totals.Facts.CacheUnknown = true
	*totals.Facts.Tier = "flex"
	*totals.Facts.Deferred.InputTokens = 30
	*totals.Facts.Credits = 5
	totals.Facts.Cost.Currency = "EUR"
	if *copy.Facts.InputTokens != 0 || *copy.Facts.CacheWrite1hTokens != 3 || *copy.Facts.CacheUnknown ||
		*copy.Facts.Tier != "" || *copy.Facts.Deferred.InputTokens != 12 || *copy.Facts.Credits != 0 ||
		copy.Facts.Cost.Currency != "USD" {
		t.Fatalf("normalized usage retained mutable sender pointers: %+v", copy.Facts)
	}
	if (*Usage)(nil).Clone() != nil {
		t.Fatal("an absent snapshot became a zero snapshot")
	}
}

func TestExecutionUsageFactsAdmissionAndBoundaries(t *testing.T) {
	manifest := Manifest{Contract: FullContract()}
	for _, sample := range []struct {
		name string
		use  *Usage
	}{
		{"negative", &Usage{Facts: &ExecutionUsageFacts{ExecutionCounts: ExecutionCounts{InputTokens: executionFact(int64(-1))}}}},
		{"unsafe integer", &Usage{Facts: &ExecutionUsageFacts{ExecutionCounts: ExecutionCounts{InputTokens: executionFact(int64(1 << 53))}}}},
		{"cache subset", &Usage{Facts: &ExecutionUsageFacts{ExecutionCounts: ExecutionCounts{
			CacheWriteTokens: executionFact(int64(2)), CacheWrite1hTokens: executionFact(int64(3))}}}},
		{"deferred negative", &Usage{Facts: &ExecutionUsageFacts{Deferred: &ExecutionCounts{OutputTokens: executionFact(int64(-1))}}}},
		{"mixed counts", &Usage{InputTokens: 1, Facts: &ExecutionUsageFacts{}}},
		{"mixed cost", &Usage{Cost: 1, Facts: &ExecutionUsageFacts{}}},
		{"mixed currency", &Usage{Currency: "USD", Facts: &ExecutionUsageFacts{}}},
		{"credits infinity", &Usage{Facts: &ExecutionUsageFacts{Credits: executionFact(math.Inf(1))}}},
		{"credits negative", &Usage{Facts: &ExecutionUsageFacts{Credits: executionFact(-1.0)}}},
		{"cost missing currency", &Usage{Facts: &ExecutionUsageFacts{Cost: &ExecutionCost{Amount: 1}}}},
		{"cost negative", &Usage{Facts: &ExecutionUsageFacts{Cost: &ExecutionCost{Amount: -1, Currency: "USD"}}}},
		{"cost nan", &Usage{Facts: &ExecutionUsageFacts{Cost: &ExecutionCost{Amount: math.NaN(), Currency: "USD"}}}},
		{"cost oversized currency", &Usage{Facts: &ExecutionUsageFacts{Cost: &ExecutionCost{Currency: strings.Repeat("x", 17)}}}},
		{"metadata newline", &Usage{Facts: &ExecutionUsageFacts{ServedModel: executionFact("model\nforged")}}},
		{"metadata long", &Usage{Facts: &ExecutionUsageFacts{RequestID: executionFact(strings.Repeat("x", 301))}}},
		{"legacy invalid cost", &Usage{Cost: math.Inf(1)}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			if err := ValidateExecutionUsage(manifest, sample.use); err == nil {
				t.Fatalf("invalid execution usage was admitted: %+v", sample.use)
			}
		})
	}
	manifest.Contract.Version.Minor = 6
	if err := ValidateExecutionUsage(manifest, &Usage{Facts: &ExecutionUsageFacts{}}); err == nil {
		t.Fatal("an earlier module acquired an unnegotiated usage schema")
	}
	for _, legacy := range []*Usage{nil, {}, {InputTokens: 12, OutputTokens: 3, CachedTokens: 2, CacheWriteTokens: 1, Cost: 0.5, Currency: "USD"}} {
		if err := ValidateExecutionUsage(manifest, legacy); err != nil {
			t.Fatalf("legacy cumulative snapshot changed: %v", err)
		}
	}
}
