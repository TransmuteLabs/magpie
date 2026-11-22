package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// #2333 (zhenghaowen0967): magpie quota --json and GET /v1/magpie/quotas
// tell each window's whole as the Usage page's card does, reckoned once
// over every card, and say why where it isn't told.
func TestQuotaReportTellsWindowHolds(t *testing.T) {
	was := ReckonHolds
	t.Cleanup(func() { ReckonHolds = was })
	calls, seen := 0, 0
	ReckonHolds = func(_ context.Context, qs []SubscriptionQuota, now time.Time) []SubscriptionQuota {
		calls++
		seen = len(qs)
		out := make([]SubscriptionQuota, len(qs))
		for i, q := range qs {
			q.Windows = append([]QuotaWindow(nil), q.Windows...)
			for j := range q.Windows {
				switch q.Windows[j].Name {
				case "5 hours":
					q.Windows[j].Holds = &WindowHolds{Tokens: 1200000, Cost: 4.5, Priced: true, Used: 20}
				case "7 days":
					q.Windows[j].HoldsWhy = HoldsLow
				}
			}
			out[i] = q
		}
		return out
	}
	subs := []SubscriptionQuota{{Provider: "codex", User: "sam@example.com", Windows: []QuotaWindow{{Name: "5 hours", Used: 20}, {Name: "7 days", Used: 3}}}}
	plans := []SubscriptionQuota{{Provider: "glm", User: "Key", Windows: []QuotaWindow{{Name: "Monthly", Used: 9}}}}
	balances := []SubscriptionQuota{{Provider: "deepseek", Balance: "$5.00"}}
	r := quotaReport(t.Context(), subs, plans, balances, time.Now())
	if calls != 1 || seen != 3 {
		t.Fatalf("reckoned %d times over %d cards, want once over all 3", calls, seen)
	}
	if len(r) != 3 || r[0].Kind != "subscription" || r[1].Kind != "plan" || r[2].Kind != "balance" {
		t.Fatalf("cards out of their groups: %+v", r)
	}
	w := r[0].Windows
	if h := w[0].Holds; h == nil || h.Tokens != 1200000 || h.Cost != 4.5 || !h.Priced {
		t.Fatalf("5 hours told no whole: %+v", w[0])
	}
	if w[1].Holds != nil || w[1].HoldsWhy != HoldsLow {
		t.Fatalf("7 days: %+v, want no whole and why %q", w[1], HoldsLow)
	}
	b, _ := json.Marshal(r[0])
	for _, k := range []string{`"holds":{"tokens":1200000,"cost":4.5,"priced":true`, `"holdsWhy":"low"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("JSON has no %s: %s", k, b)
		}
	}
	if b, _ := json.Marshal(r[1]); strings.Contains(string(b), "holds") {
		t.Errorf("a window told nothing says holds: %s", b)
	}
	scoped := buildQuotaReport(t.Context(), subs, plans, balances, time.Now(), func(_ context.Context, id, _ string) bool {
		return id == "codex"
	})
	if calls != 1 || len(scoped) != 1 || scoped[0].Provider != "codex" || len(scoped[0].Windows) != 2 {
		t.Fatalf("filtered quota report = %+v, history scans = %d", scoped, calls)
	}
	for _, window := range scoped[0].Windows {
		if window.Holds != nil || window.HoldsWhy != "" {
			t.Errorf("scoped quota returned a hold: %+v", window)
		}
	}

	// where usage isn't linked in, no whole and no reason
	ReckonHolds = nil
	if r := quotaReport(t.Context(), subs, nil, nil, time.Now()); r[0].Windows[0].Holds != nil || r[0].Windows[1].HoldsWhy != "" {
		t.Fatalf("told without a reckoner: %+v", r[0].Windows)
	}
}

func TestQuotaReportRechecksSubscriptionAfterReckoning(t *testing.T) {
	for _, variant := range []struct {
		name   string
		report func(context.Context, time.Time) []Quota
	}{
		{"current", QuotaReport},
		{"kept", KeptQuotaReportContext},
	} {
		t.Run(variant.name, func(t *testing.T) {
			host, _, calls := prepareUsageReadFixture(t)
			id := ExtensionProviderID(host.manifest.ID, "vendor")
			alice := usageTestRead(t, id, "alice", 20)
			bob := SubscriptionQuota{Provider: "other-subscription", User: "bob", Windows: []QuotaWindow{{Name: "quota", Used: 30}}}
			warmSubscriptionUsageForTest(t, alice, bob)
			before := calls.Load()
			previous := ReckonHolds
			t.Cleanup(func() { ReckonHolds = previous })
			ReckonHolds = func(_ context.Context, cards []SubscriptionQuota, _ time.Time) []SubscriptionQuota {
				quota := cardOf(cards, id, "alice")
				if quota == nil || len(quota.Windows) != 1 || quota.Windows[0].Used != 20 {
					t.Fatalf("the revoked reading was absent before reckoning: %+v", quota)
				}
				if _, err := host.Store().InvalidateUsage(alice.extensionUsage.stamp.Scope); err != nil {
					t.Fatal(err)
				}
				return cards
			}
			var sawAlice, sawBob bool
			for _, quota := range variant.report(t.Context(), time.Now()) {
				switch {
				case quota.Provider == id && quota.User == "alice":
					sawAlice = quota.ReadDeferred && len(quota.Windows) == 0
				case quota.Provider == bob.Provider && quota.User == bob.User:
					sawBob = !quota.ReadDeferred && len(quota.Windows) == 1 && quota.Windows[0].Used == 30
				}
			}
			if !sawAlice || !sawBob || calls.Load() != before {
				t.Fatalf("final report did not defer the revoked reading and retain its sibling: alice=%t bob=%t vendor calls=%d", sawAlice, sawBob, calls.Load()-before)
			}
		})
	}
}

func TestQuotaReportRechecksSubscriptionAfterAccountScope(t *testing.T) {
	for _, kept := range []bool{false, true} {
		name := "current"
		if kept {
			name = "kept"
		}
		t.Run(name, func(t *testing.T) {
			host, _, calls := prepareUsageReadFixture(t)
			id := ExtensionProviderID(host.manifest.ID, "vendor")
			alice := usageTestRead(t, id, "alice", 20)
			bob := usageTestRead(t, id, "bob", 20)
			hidden := SubscriptionQuota{Provider: "other-subscription", User: "charlie", Windows: []QuotaWindow{{Name: "quota", Used: 40}}}
			warmSubscriptionUsageForTest(t, alice, bob, hidden)
			before := calls.Load()
			checkedAlice := false
			report := ScopedQuotaReport(t.Context(), time.Now(), kept, func(_ context.Context, provider, user string) bool {
				switch {
				case provider == id && user == "alice":
					checkedAlice = true
				case provider == bob.Provider && user == bob.User:
					if !checkedAlice {
						t.Fatal("the sibling was scoped before the SDK card")
					}
					if _, err := host.Store().InvalidateUsage(alice.extensionUsage.stamp.Scope); err != nil {
						t.Fatal(err)
					}
				}
				return user != hidden.User
			})
			if len(report) != 2 || report[0].Provider != id || report[0].User != "alice" ||
				!report[0].ReadDeferred || len(report[0].Windows) != 0 || report[1].Provider != id || report[1].User != bob.User ||
				report[1].ReadDeferred || len(report[1].Windows) != 1 || report[1].Windows[0].Used != bob.Windows[0].Used ||
				calls.Load() != before {
				t.Fatalf("scoped report = %+v, vendor calls = %d; want deferred Alice and current Bob", report, calls.Load()-before)
			}
		})
	}
}
