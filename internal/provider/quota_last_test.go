package provider

import (
	"context"
	"testing"
	"time"
)

func TestKeepLast(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	restart := func() {
		lastQuotas.Lock()
		lastQuotas.m, lastQuotas.loaded = nil, false
		lastQuotas.Unlock()
	}
	restart()
	defer restart()
	later := time.Now().Add(time.Hour)
	good := SubscriptionQuota{Provider: "claude", Name: "Claude Code", User: "A@x.com", Windows: []QuotaWindow{
		{Name: "5 hours", Used: 40, ResetsAt: &later, Span: 5 * time.Hour},
		{Name: "7 days · Opus", Used: 10, Model: "opus", Span: 7 * 24 * time.Hour},
	}}
	if q := keepLast(good, ""); q.AsOf != nil || q.Windows[0].Used != 40 {
		t.Fatalf("%+v", q)
	}
	restart() // what was read is on disk

	limited := SubscriptionQuota{Provider: "claude", Name: "Claude Code", User: "a@x.com", Windows: []QuotaWindow{},
		Error: errClaudeNotAsked.Error()}
	q := keepLast(limited, "")
	if q.Error != "" || q.AsOf == nil || len(q.Windows) != 2 || q.Windows[0].Used != 40 || q.Windows[1].Model != "opus" || q.Windows[0].Span != 5*time.Hour {
		t.Fatalf("not asked: %+v", q)
	}

	// the same through LoginUsage, which knows the user but not the name
	q = keepLast(SubscriptionQuota{Provider: "claude", Windows: []QuotaWindow{}, Error: "Too Many Requests"}, "a@x.com")
	if q.Error != "" || q.Name != "Claude Code" || q.Windows[0].Used != 40 {
		t.Fatalf("login: %+v", q)
	}

	// a sign-in gone bad is said, not covered up
	if q := keepLast(SubscriptionQuota{Provider: "claude", User: "a@x.com", Error: "Claude Code's sign-in has expired"}, ""); q.Error == "" {
		t.Fatalf("expired: %+v", q)
	}

	// a new reading is used at once
	good.Windows[0].Used = 70
	keepLast(good, "")
	if q := keepLast(limited, ""); q.Windows[0].Used != 70 {
		t.Fatalf("newer: %+v", q)
	}

	// a window that has reset since starts again from nothing
	past := time.Now().Add(-time.Minute)
	good.Windows[0].ResetsAt = &past
	keepLast(good, "")
	if q := keepLast(limited, ""); q.Windows[0].Used != 0 {
		t.Fatalf("reset: %+v", q)
	}
}

func TestPartialKeepsCompleteFallbackAndResetBarrier(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ForgetKeptCardsForTest()
	t.Cleanup(ForgetKeptCardsForTest)
	now := time.Now()
	good := SubscriptionQuota{Provider: "zcode", User: "a", AccountIdentity: "scope", AccountGeneration: "epoch",
		Windows: []QuotaWindow{{Name: "Paid", Used: 35}}}
	keepLast(good, "")
	partial := SubscriptionQuota{Provider: "zcode", User: "a", AccountIdentity: "scope", AccountGeneration: "epoch",
		Error: "HTTP 503", Partial: true, ReadAt: &now, Windows: []QuotaWindow{{Name: "Start", Used: 70}}}
	got := keepLast(partial, "")
	if !got.Partial || got.Error != partial.Error || len(got.Windows) != 1 || got.Windows[0].Name != "Start" {
		t.Fatalf("partial replaced by fallback: %+v", got)
	}
	failed := good
	failed.Windows, failed.Error = nil, "HTTP 503"
	kept := keepLast(failed, "")
	if kept.AsOf == nil || len(kept.Windows) != 1 || kept.Windows[0].Name != "Paid" || kept.Windows[0].Used != 35 {
		t.Fatalf("partial replaced the complete fallback: %+v", kept)
	}
	key := quotaLastKey(partial, partial.User)
	quotaReadBarriers.Lock()
	if quotaReadBarriers.m == nil {
		quotaReadBarriers.m = map[string]uint64{}
	}
	quotaReadBarriers.m[key] = 2
	quotaReadBarriers.Unlock()
	t.Cleanup(func() {
		quotaReadBarriers.Lock()
		delete(quotaReadBarriers.m, key)
		quotaReadBarriers.Unlock()
	})
	partial.Observations = []UsageObservation{{Kind: "server-mcp", Name: "Server MCP"}}
	observationOnly := partial
	observationOnly.Windows, observationOnly.ReadAt = nil, nil
	stamped := readNow(observationOnly)
	if stamped.ReadAt == nil || stamped.Error != partial.Error || !stamped.Partial {
		t.Fatalf("observation-only read was not stamped: %+v", stamped)
	}
	if again := readNow(stamped); again.ReadAt == nil || !again.ReadAt.Equal(*stamped.ReadAt) {
		t.Fatalf("partial was restamped: %+v", again)
	}
	for _, stale := range []SubscriptionQuota{
		{Error: partial.Error, Partial: true, AsOf: &now, Observations: partial.Observations},
		{Error: partial.Error, Partial: true, ReadDeferred: true, Observations: partial.Observations},
	} {
		if read := readNow(stale); read.ReadAt != nil {
			t.Fatalf("stale or deferred subset was stamped as fresh: %+v", read)
		}
	}
	invalid := keepReading(context.WithValue(context.Background(), quotaReadKey{}, uint64(1)), partial, "")
	if invalid.Partial || len(invalid.Windows) != 0 || len(invalid.Observations) != 0 || invalid.ReadAt != nil ||
		invalid.AccountIdentity != "scope" || invalid.AccountGeneration != "epoch" || invalid.Error == "" {
		t.Fatalf("superseded subset remained fresh: %+v", invalid)
	}
}
