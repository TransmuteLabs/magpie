package provider

import (
	"strings"
	"testing"
	"time"
)

func TestStatuslineIncludesFreshnessAndFailures(t *testing.T) {
	at := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		card Quota
		want string
	}{
		{"error", Quota{Error: "unavailable"}, "[error: unavailable]"},
		{"cached", Quota{AsOf: &at}, "[as of " + at.Local().Format("2006-01-02 15:04") + "]"},
		{"deferred", Quota{ReadDeferred: true}, "[not refreshed]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := tc.card
			q.Provider, q.Kind = "sample", "subscription"
			cards := StatuslineCards([]Quota{q}, nil)
			if len(cards) != 1 {
				t.Fatalf("status-only card was omitted: %+v", cards)
			}
			if got := StatuslineText("", cards, nil, at); !strings.Contains(got, tc.want) {
				t.Fatalf("line %q lacks %q", got, tc.want)
			}
			q.Kind = "balance"
			if got := StatuslineCards([]Quota{q}, nil); len(got) != 0 {
				t.Fatalf("balance-only card was included: %+v", got)
			}
		})
	}
}

func TestStatuslineFreshnessKeepsWindowsAndSelection(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	stale := Quota{Provider: "sample", User: "alice", Alias: "main", Kind: "subscription", Error: "offline", AsOf: &past, ReadDeferred: true,
		Windows: []QuotaSpan{{Name: "5 hours", Used: 62}, {Name: "7 days", Used: 80, ResetsAt: &past}}}
	line := StatuslineCard(stale, now)
	for _, want := range []string{"sample main", "[error: offline]", "[as of ", "[not refreshed]", "5h 62%", "7d 80%"} {
		if !strings.Contains(line, want) {
			t.Errorf("%q lacks %q", line, want)
		}
	}
	served := Quota{Provider: "served", Kind: "subscription", Error: "unavailable", LastServedAt: &now}
	cards := []Quota{stale, served, {Provider: "empty", Kind: "subscription"}}
	if got := StatuslineCards(cards, nil); len(got) != 1 || got[0].Provider != "served" {
		t.Fatalf("lately served preference changed: %+v", got)
	}
	if got := StatuslineCards(cards, []string{"main"}); len(got) != 1 || got[0].Provider != "sample" {
		t.Fatalf("explicit selection changed: %+v", got)
	}
}

func TestStatuslineExpiredWindowRequiresFreshReading(test *testing.T) {
	now := time.Date(2026, time.November, 12, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	for _, scenario := range []struct {
		name     string
		resetAt  *time.Time
		asOf     *time.Time
		deferred bool
		want     string
	}{
		{name: "fresh-expired", resetAt: &past, want: "7d 0%"},
		{name: "fresh-at-reset", resetAt: &now, want: "7d 0%"},
		{name: "fresh-future-reset", resetAt: &future, want: "7d 80%"},
		{name: "fresh-unknown-reset", want: "7d 80%"},
		{name: "stale-expired", resetAt: &past, asOf: &past, want: "7d 80%"},
		{name: "stale-at-reset", resetAt: &now, asOf: &past, want: "7d 80%"},
		{name: "deferred-expired", resetAt: &past, deferred: true, want: "7d 80%"},
		{name: "deferred-at-reset", resetAt: &now, deferred: true, want: "7d 80%"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			quota := Quota{
				Provider: "sample", Kind: "subscription", AsOf: scenario.asOf, ReadDeferred: scenario.deferred,
				Windows: []QuotaSpan{{Name: "7 days", Used: 80, ResetsAt: scenario.resetAt}},
			}
			if line := StatuslineCard(quota, now); !strings.HasSuffix(line, scenario.want) {
				test.Errorf("card %q, want suffix %q", line, scenario.want)
			}
			if line := StatuslineText("model", []Quota{quota}, nil, now); !strings.HasSuffix(line, scenario.want) {
				test.Errorf("statusline %q, want suffix %q", line, scenario.want)
			}
		})
	}
}
