package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRemoteCardsCacheSeparatesEndpoints(t *testing.T) {
	ForgetRemoteCardsForTest()
	t.Cleanup(ForgetRemoteCardsForTest)
	old := Provider{ID: "office", Anthropic: "https://user:secret@old.invalid", Key: "synthetic-gateway-key"}
	current := old
	current.Anthropic = "https://new.invalid"
	oldID, currentID := remoteCardsID(old), remoteCardsID(current)
	if oldID == currentID {
		t.Fatal("different remote endpoints share a cache entry")
	}
	for _, secret := range []string{old.Anthropic, "secret", old.Key} {
		if strings.Contains(oldID, secret) {
			t.Fatalf("cache key exposes %q", secret)
		}
	}
	now := time.Now()
	keepRemoteCards(currentID, now, []SubscriptionQuota{{Provider: "sample", Name: "Current"}}, nil)
	keepRemoteCards(oldID, now.Add(time.Second), []SubscriptionQuota{{Provider: "sample", Name: "Old"}}, nil)
	got := remoteCards(context.Background(), current, false)
	if len(got) != 1 || !strings.HasPrefix(got[0].Name, "Current") {
		t.Fatalf("late old-endpoint reply replaced current endpoint's cards: %+v", got)
	}
}

func TestRemoteCardsFailedReadKeepsOldestTime(t *testing.T) {
	ForgetRemoteCardsForTest()
	t.Cleanup(ForgetRemoteCardsForTest)
	p := Provider{ID: "office", Anthropic: "https://office.invalid", Key: "synthetic"}
	now := time.Now()
	readAt, older := now.Add(-4*time.Hour), now.Add(-8*time.Hour)
	raw := []SubscriptionQuota{
		{Provider: "sample", Name: "Read", ReadAt: &readAt, Windows: []QuotaWindow{{Name: "5 hours", Used: 91}}},
		{Provider: "sample", Name: "Stale", ReadAt: &readAt, AsOf: &older, ReadDeferred: true},
		{Provider: "sample", Name: "Unknown"},
	}
	goodAt := now.Add(-time.Minute)
	keepRemoteCards(remoteCardsID(p), goodAt, raw, nil)
	keepRemoteCards(remoteCardsID(p), now, nil, errors.New("unavailable"))
	got := remoteCards(context.Background(), p, false)
	wantTimes := []time.Time{readAt, older, goodAt}
	for i, want := range wantTimes {
		if got[i].AsOf == nil || !got[i].AsOf.Equal(want) || got[i].ReadDeferred {
			t.Errorf("failed read's card %d lost provenance: %+v, want %v", i, got[i], want)
		}
	}
	if raw[0].AsOf != nil || !raw[1].ReadDeferred {
		t.Fatalf("failed read mutated cached cards: %+v", raw)
	}
	keepRemoteCards(remoteCardsID(p), now.Add(-time.Second), []SubscriptionQuota{{Provider: "late"}}, nil)
	if cards := remoteCards(context.Background(), p, false); cards[0].AsOf == nil {
		t.Fatal("late successful read cleared the newer failure")
	}
	keepRemoteCards(remoteCardsID(p), now.Add(time.Second), []SubscriptionQuota{{Provider: "sample", Name: "Recovered"}}, nil)
	if cards := remoteCards(context.Background(), p, false); cards[0].AsOf != nil || cards[0].Error != "" {
		t.Fatalf("successful read did not recover freshness: %+v", cards)
	}
}

func TestNamedRemoteCardsCopiesWindowsAndResets(t *testing.T) {
	raw := []SubscriptionQuota{{
		Provider: "codex", Windows: []QuotaWindow{{Name: "5 hours", Used: 60}},
		Resets: &ResetCredits{Count: 1, Each: []ResetCard{{ID: "original"}}},
	}}
	got := namedRemoteCards(Provider{ID: "office"}, raw)
	got[0].Windows[0].Used = 1
	got[0].Resets.Count = 0
	got[0].Resets.Each[0].ID = "changed"
	if raw[0].Windows[0].Used != 60 || raw[0].Resets.Count != 1 || raw[0].Resets.Each[0].ID != "original" {
		t.Fatalf("returned card mutated the cache owner: %+v, %+v", raw[0], raw[0].Resets)
	}
}

func TestPassedOnRemoteCardsKeepFailedReadTime(t *testing.T) {
	providerFileHome(t)
	ForgetRemoteCardsForTest()
	t.Cleanup(ForgetRemoteCardsForTest)
	if err := Save(Provider{ID: "office", Preset: RemoteMagpiePreset, Chat: "https://office.invalid", Key: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	p, err := Find("office")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	at := now.Add(-time.Hour)
	id := remoteCardsID(*p)
	keepRemoteCards(id, at, []SubscriptionQuota{{Provider: "sample", Name: "Sample"}}, nil)
	keepRemoteCards(id, now, nil, errors.New("unavailable"))
	got := passedOnCards()
	if len(got) != 1 || got[0].AsOf == nil || !got[0].AsOf.Equal(at) || got[0].ReadDeferred {
		t.Fatalf("forwarded card hid its failed read: %+v", got)
	}
}
