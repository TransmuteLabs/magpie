package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
)

// cardOf is the card cs has of provider and user, nil when none.
func cardOf(cs []SubscriptionQuota, provider, user string) *SubscriptionQuota {
	for i := range cs {
		if cs[i].Provider == provider && cs[i].User == user {
			return &cs[i]
		}
	}
	return nil
}

// #1313 (jorben): a magpie serving only other magpies has nobody on its
// Usage page, while its Codex accounts' usage is read all the time behind
// the requests (the account switching, CodexUsedUp). What another magpie
// is told of its cards (CachedCards) is that reading, not "nothing read";
// after a restart, before anything is read again, it is the reading kept
// on disk, dated. Telling it asks nobody.
func TestCachedCardsTellBackgroundReadings(t *testing.T) {
	signIn(t) // Codex on me@example.com
	rememberLogins(true)
	var asked atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		asked.Add(1)
		w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,
			"primary_window":{"used_percent":37,"limit_window_seconds":18000,"reset_after_seconds":3600},
			"secondary_window":{"used_percent":12,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`))
	}))
	defer fake.Close()
	old := CodexBase
	CodexBase = fake.URL + "/backend-api/codex"
	t.Cleanup(func() { CodexBase = old })
	forget := func() {
		loginUsageCache.Lock()
		loginUsageCache.m = nil
		loginUsageCache.Unlock()
		subscriptionUsageCache.Lock()
		subscriptionUsageCache.data, subscriptionUsageCache.at = nil, time.Time{}
		subscriptionUsageCache.Unlock()
		ForgetKeptCardsForTest()
	}
	forget()
	t.Cleanup(forget)

	if q := cardOf(CachedCards(time.Now()), "codex", "me@example.com"); q != nil {
		t.Fatalf("a card before anything was read: %+v", q)
	}
	if CodexUsedUp(context.Background()) {
		t.Fatal("held at 37%")
	}
	used := func(q *SubscriptionQuota) float64 {
		if q == nil || len(q.Windows) == 0 {
			return -1
		}
		return q.Windows[0].Used
	}
	q := cardOf(CachedCards(time.Now()), "codex", "me@example.com")
	if used(q) != 37 || q.Name != "Codex" || q.Kind != "subscription" || q.AsOf != nil || q.Error != "" {
		t.Fatalf("after the background read: %+v", q)
	}
	// restarted: nothing read yet, the reading kept on disk, dated
	loginUsageCache.Lock()
	loginUsageCache.m = nil
	loginUsageCache.Unlock()
	ForgetKeptCardsForTest()
	q = cardOf(CachedCards(time.Now()), "codex", "me@example.com")
	if used(q) != 37 || q.AsOf == nil || q.Name != "Codex" {
		t.Fatalf("after a restart: %+v", q)
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("ChatGPT asked %d times, want 1 (the background read)", n)
	}
}

// The page's card stands when it was read after the background's reading.
func TestCachedCardsKeepALaterPageRead(t *testing.T) {
	signIn(t)
	rememberLogins(true)
	t.Cleanup(func() {
		loginUsageCache.Lock()
		loginUsageCache.m = nil
		loginUsageCache.Unlock()
		subscriptionUsageCache.Lock()
		subscriptionUsageCache.data = nil
		subscriptionUsageCache.Unlock()
		ForgetKeptCardsForTest()
	})
	loginUsageCache.Lock()
	loginUsageCache.m = map[string]loginUsageEntry{"codex/me@example.com": {at: time.Now(),
		read: SubscriptionQuota{Provider: "codex", User: "me@example.com", Windows: []QuotaWindow{{Name: "5h", Used: 10}}, readSeq: 5}}}
	loginUsageCache.Unlock()
	subscriptionUsageCache.Lock()
	subscriptionUsageCache.data = []SubscriptionQuota{{Provider: "codex", Name: "Codex", User: "me@example.com",
		Windows: []QuotaWindow{{Name: "5h", Used: 20}}, readSeq: 6}}
	subscriptionUsageCache.Unlock()
	if q := cardOf(CachedCards(time.Now()), "codex", "me@example.com"); q == nil || q.Windows[0].Used != 20 {
		t.Fatalf("the later page read: %+v", q)
	}
	subscriptionUsageCache.Lock()
	subscriptionUsageCache.data[0].readSeq = 4
	subscriptionUsageCache.Unlock()
	if q := cardOf(CachedCards(time.Now()), "codex", "me@example.com"); q == nil || q.Windows[0].Used != 10 || q.Name != "Codex" {
		t.Fatalf("the later background read: %+v", q)
	}
}

func TestCachedCardsUseCurrentExtensionBackgroundReading(t *testing.T) {
	host, used, calls := prepareUsageReadFixture(t)
	id := ExtensionProviderID(host.manifest.ID, "vendor")
	before := usageTestRead(t, id, "alice", 20)
	warmSubscriptionUsageForTest(t)
	if card := cardOf(CachedCards(time.Now()), id, "alice"); card == nil ||
		card.Name != host.manifest.Providers[0].Name || len(card.Windows) != 1 || card.Windows[0].Used != 20 {
		t.Errorf("background SDK read did not create a subscription card: %+v", card)
	}
	warmSubscriptionUsageForTest(t, before)
	logins, _ := extensionLogins(id)
	forgetLoginReading(logins[0])
	used.Store(40)
	usageTestRead(t, id, "alice", 40)
	beforeCards := calls.Load()
	if card := cardOf(CachedCards(time.Now()), id, "alice"); card == nil || len(card.Windows) != 1 || card.Windows[0].Used != 40 {
		t.Errorf("older page read overshadowed a current background SDK read: %+v", card)
	}
	if _, err := extensionhost.NewStore(host.Namespace()).InvalidateUsage(logins[0].extension.currentScope()); err != nil {
		t.Fatal(err)
	}
	cards := CachedCards(time.Now())
	if card := cardOf(cards, id, "alice"); card != nil && len(card.Windows) != 0 {
		t.Errorf("invalidated SDK reading still became a known card: %+v", card)
	}
	if card := cardOf(cards, id, "bob"); card == nil || len(card.Windows) != 1 || card.Windows[0].Used != 20 {
		t.Errorf("alice's invalidation discarded bob's current card: %+v", card)
	}
	if calls.Load() != beforeCards {
		t.Fatal("cached card publication asked a vendor")
	}
}

func TestCachedCardsIgnoreLegacySnapshotAfterCutover(t *testing.T) {
	host, _, calls := prepareUsageReadFixture(t)
	const publicID = "transferred-subscription"
	page := keepLast(SubscriptionQuota{Provider: publicID, User: "alice", Windows: []QuotaWindow{{Name: "legacy", Used: 80}}}, "alice")
	usageTestSubscriptionOwner(t, host, publicID)
	warmSubscriptionUsageForTest(t)
	if card := cardOf(CachedCards(time.Now()), publicID, "alice"); card != nil && len(card.Windows) != 0 {
		t.Errorf("SDK card inherited a legacy disk snapshot: %+v", card)
	}
	warmSubscriptionUsageForTest(t, page)
	if card := cardOf(CachedCards(time.Now()), publicID, "alice"); card != nil && len(card.Windows) != 0 {
		t.Errorf("SDK card inherited a legacy page snapshot: %+v", card)
	}
	if calls.Load() != 0 {
		t.Fatal("cached card publication asked a vendor during cutover")
	}
}

func TestSubscriptionUsageRejectsLegacyCardAfterCutover(t *testing.T) {
	host, _, calls := prepareUsageReadFixture(t)
	const publicID = "transferred-subscription"
	until := time.Now().Add(time.Hour)
	page := SubscriptionQuota{
		Provider: publicID, Name: "Moved subscription", User: "alice", Plan: "legacy plan",
		Windows: []QuotaWindow{{Name: "legacy", Used: 80}}, Balance: "$2",
		BalanceTrend: &BalanceTrend{}, Until: &until, Renew: "auto",
		Resets: &ResetCredits{Count: 2},
	}
	resetsOnly := SubscriptionQuota{Provider: publicID, Name: page.Name, User: "old-account", Resets: &ResetCredits{Count: 1}}
	other := SubscriptionQuota{Provider: "other-subscription", User: "bob", Windows: []QuotaWindow{{Name: "quota", Used: 30}}}
	warmSubscriptionUsageForTest(t, page, resetsOnly, other)
	if card := cardOf(SubscriptionUsage(t.Context()), publicID, "alice"); card == nil || len(card.Windows) != 1 || card.Windows[0].Used != 80 {
		t.Fatalf("the Usage page did not show the current legacy card: %+v", card)
	}

	usageTestSubscriptionOwner(t, host, publicID)
	for source, cards := range map[string][]SubscriptionQuota{
		"Usage page": SubscriptionUsage(t.Context()),
		"export":     CachedCardsContext(t.Context(), time.Now()),
	} {
		for _, user := range []string{"alice", "old-account"} {
			card := cardOf(cards, publicID, user)
			if card == nil || card.Name != page.Name || len(card.Windows) != 0 || !card.ReadDeferred {
				t.Errorf("%s did not defer the old owner's card for %s: %+v", source, user, card)
				continue
			}
			data, err := json.Marshal(card)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"plan", "balance", "balanceTrend", "until", "renew", "resets"} {
				if _, found := fields[field]; found {
					t.Errorf("%s published stale %s for %s", source, field, user)
				}
			}
		}
		if card := cardOf(cards, other.Provider, other.User); card == nil || len(card.Windows) != 1 || card.Windows[0].Used != 30 {
			t.Errorf("%s changed an unrelated subscription: %+v", source, card)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("publishing cached quotas asked a vendor")
	}
}

// Two keys of one provider given the same name are two cards: each is
// read and kept by its key (keyTag), and another magpie is told both
// after a restart, as the Usage page shows two. cardKeys answered a map
// keyed by the name, so of one name it held the last key alone, and the
// first key's reading was told as nobody's.
func TestCachedCardsKeepBothKeysOfOneName(t *testing.T) {
	keyLimitsHome(t)
	p := Provider{ID: "relay", Name: "Relay", Chat: "https://relay.example/v1", Responses: "https://relay.example/v1",
		Key: "sk-one-key", KeyName: "main", Keys: []KeyAccount{{Key: "sk-two-key", Name: "main"}}}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	// what the Usage page read of each key, kept for the restart (keepLast,
	// as KeyBalances keeps a card: by the key, under the name it showed)
	for i, key := range []string{"sk-one-key", "sk-two-key"} {
		keepLast(SubscriptionQuota{Provider: "relay", Name: "Relay", User: "main",
			Balance: fmt.Sprintf("$%d.00", i+1)}, keyTag("balance", key))
	}
	forget := func() {
		planQuotaCache.Lock()
		planQuotaCache.data = nil
		planQuotaCache.Unlock()
		keyBalanceCache.Lock()
		keyBalanceCache.data = nil
		keyBalanceCache.Unlock()
	}
	forget()
	t.Cleanup(func() {
		forget()
		lastQuotas.Lock()
		lastQuotas.m, lastQuotas.loaded = nil, false
		lastQuotas.Unlock()
	})
	var got []string
	for _, q := range CachedCards(time.Now()) {
		if q.Provider == "relay" && q.Kind == "balance" {
			got = append(got, q.User+" "+q.Balance)
		}
	}
	if len(got) != 2 || got[0] != "main $1.00" || got[1] != "main $2.00" {
		t.Fatalf("both keys' kept readings, got %v", got)
	}
}

// The sibling for keys: routing reads a sub2api key's windows behind the
// requests (KeyAllowance); another magpie is told them on the key's card,
// one made for it when the Usage page never read it.
func TestCachedCardsTellKeyReadings(t *testing.T) {
	keyLimitsHome(t)
	week := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	srv, asked := sub2apiServer(t, func() ([]byte, int) { return weekUsed(t, "720", week), 200 })
	p := sub2apiKey(t, srv)
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	if q := cardOf(CachedCards(time.Now()), p.ID, ""); q != nil {
		t.Fatalf("a card before anything was read: %+v", q)
	}
	waitAllowance(t, p, func(Allowance) bool { return true })
	q := cardOf(CachedCards(time.Now()), p.ID, "")
	if q == nil || q.Kind != "balance" || len(q.Windows) != 2 || q.Windows[1].Name != "7 days" || q.Windows[1].Used != 90 || q.Name != "Sub2API" || q.ReadAt == nil {
		t.Fatalf("after routing's read: %+v", q)
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("sub2api asked %d times, want 1", n)
	}
}

// Routing's reading of each of two keys of one name goes on that key's
// own card, not both on the first card of the name.
func TestKeyReadingsReachBothCardsOfOneName(t *testing.T) {
	keyLimitsHome(t)
	p := Provider{ID: "sub2api", Name: "Sub2API", Chat: "https://relay.example/v1", Responses: "https://relay.example/v1",
		BalanceURL: "https://relay.example/v1/usage", Key: "sk-one-key", KeyName: "main", Keys: []KeyAccount{{Key: "sk-two-key", Name: "main"}}}
	now := time.Now()
	week := now.Add(72 * time.Hour)
	// what routing read of each key (readKeyAllowance)
	keyAllowances.Lock()
	if keyAllowances.m == nil {
		keyAllowances.m = map[string]*keyAllowance{}
	}
	for i, key := range []string{"sk-one-key", "sk-two-key"} {
		q := p
		q.Key = key
		keyAllowances.m[keyAllowanceID(q)] = &keyAllowance{read: keyReading{
			ws: []QuotaWindow{{Name: "7 days", Span: 7 * 24 * time.Hour, Used: float64(10 * (i + 1)), ResetsAt: &week}},
			at: now, seq: uint64(i + 1)}}
	}
	keyAllowances.Unlock()
	if !strings.HasPrefix(keyCardTag(p), "balance ") {
		t.Fatalf("not a balance card: %s", keyCardTag(p))
	}
	cards := []SubscriptionQuota{{Provider: "sub2api", User: "main"}, {Provider: "sub2api", User: "main"}}
	got := withKeyReadings(cards, []Provider{p}, "balance")
	if len(got) != 2 || len(got[0].Windows) != 1 || len(got[1].Windows) != 1 || got[0].Windows[0].Used != 10 || got[1].Windows[0].Used != 20 {
		t.Fatalf("each key's windows on its own card: %+v", got)
	}
}
