package usage

import (
	"math"
	"testing"
)

func TestReportedExecutionCostPreservesCurrencyAndMeasuredZero(t *testing.T) {
	for _, cost := range []*ReportedCost{
		nil, {Amount: 42, Currency: "EUR"}, {Amount: 42}, {Amount: -1, Currency: "USD"},
		{Amount: math.NaN(), Currency: "USD"}, {Amount: math.Inf(1), Currency: "USD"},
		{Amount: math.Inf(-1), Currency: "USD"},
	} {
		if override := cost.USD(); override != nil {
			t.Fatalf("unknown, non-USD or invalid reported cost became a dollar override: %+v", cost)
		}
	}
	for _, cost := range []ReportedCost{{Amount: 0, Currency: "USD"}, {Amount: 0.125, Currency: "usd"}} {
		override := cost.USD()
		if override == nil || *override != cost.Amount {
			t.Fatalf("a measured dollar amount lost its evidence: %+v", cost)
		}
		*override = 99
		if cost.Amount == 99 {
			t.Fatal("a pricing override retained mutable evidence")
		}
	}
}
