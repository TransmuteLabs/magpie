package usage

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func persistedWindowTokens(test *testing.T) int64 {
	test.Helper()
	raw, err := os.ReadFile(carryPath())
	if err != nil {
		test.Fatalf("window estimate was not persisted in its own store: %v", err)
	}
	var entries map[string]carried
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) != 1 {
		test.Fatalf("invalid persisted window estimates: %s, %v", raw, err)
	}
	for _, entry := range entries {
		return entry.Holds.Tokens
	}
	test.Fatal("persisted estimate is missing")
	return 0
}

func persistWindowEstimate(test *testing.T, at time.Time, tokens int64) {
	test.Helper()
	end := at.Add(time.Hour)
	carryHolds([]provider.SubscriptionQuota{{
		Provider: "kimi-code", User: "fixture@example.com",
		Windows: []provider.QuotaWindow{{
			Name: "7 days", Span: 7 * 24 * time.Hour, ResetsAt: &end,
			Holds: &provider.WindowHolds{Tokens: tokens},
		}},
	}}, at)
}

func TestWindowHoldsStoresHaveIndependentFlushClocks(test *testing.T) {
	for _, offset := range []time.Duration{0, time.Second, -time.Hour} {
		test.Run(offset.String(), func(test *testing.T) {
			sessionHome(test)
			carry.Lock()
			at := carry.wroteAt.Add(time.Minute)
			if carry.wroteAt.IsZero() {
				at = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
			}
			carry.Unlock()
			persistWindowEstimate(test, at, 100000)
			if got := persistedWindowTokens(test); got != 100000 {
				test.Fatalf("first store has %d tokens", got)
			}
			sessionHome(test)
			persistWindowEstimate(test, at.Add(offset), 200000)
			if got := persistedWindowTokens(test); got != 200000 {
				test.Fatalf("second store inherited the first estimate: %d", got)
			}
		})
	}
}

func TestWindowHoldsSameStoreKeepsWriteCoalescing(test *testing.T) {
	sessionHome(test)
	carry.Lock()
	at := carry.wroteAt.Add(time.Minute)
	if carry.wroteAt.IsZero() {
		at = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	}
	carry.Unlock()
	persistWindowEstimate(test, at, 100000)
	persistWindowEstimate(test, at.Add(time.Second), 200000)
	if got := persistedWindowTokens(test); got != 100000 {
		test.Fatalf("same-store writes were not coalesced: %d", got)
	}
	persistWindowEstimate(test, at.Add(31*time.Second), 200000)
	if got := persistedWindowTokens(test); got != 200000 {
		test.Fatalf("coalesced estimate was not flushed: %d", got)
	}
}
