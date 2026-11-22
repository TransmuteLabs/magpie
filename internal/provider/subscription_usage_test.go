package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestCodexQuotaUsesProviderWindowDuration(t *testing.T) {
	for _, tt := range []struct {
		seconds int64
		want    string
	}{{5 * 60 * 60, "5 hours"}, {7 * 24 * 60 * 60, "7 days"}, {0, "Allowance"}} {
		window := codexWindow{UsedPercent: new(float64(16)), LimitWindowSecs: tt.seconds}
		if got := window.window(); got.Name != tt.want || got.Used != 16 {
			t.Errorf("Codex window for %d seconds = %+v, want %q at 16%%", tt.seconds, got, tt.want)
		}
	}
}

func TestCompactQuotaNumber(t *testing.T) {
	for _, tt := range []struct {
		in   float64
		want string
	}{{7, "7"}, {7.5, "7.5"}, {7.25, "7.25"}} {
		if got := compactNumber(tt.in); got != tt.want {
			t.Errorf("compactNumber(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A stale copy comes back at once while it refreshes, and accounts removed
// from magpie in the meantime are left out of it.
func TestSubscriptionUsageServesStale(t *testing.T) {
	signIn(t)
	if err := Delete("codex"); err != nil {
		t.Fatal(err)
	}
	// The refresh is held at the vendor, so a copy that waited for it could
	// not come back: what the stale copy returns, it returns while the
	// refresh is still in flight (#1586). Held rather than timed, because a
	// loaded machine makes a wall-clock bound say nothing either way.
	held := make(chan struct{})
	reached := make(chan struct{})
	var releaseOnce, reachedOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(held) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reachedOnce.Do(func() { close(reached) })
		<-held
		w.Write([]byte(`{"copilot_plan":"individual","token_based_billing":true,"quota_snapshots":` +
			`{"premium_interactions":{"has_quota":true,"unlimited":false,"entitlement":"300","percent_remaining":50}}}`))
	}))
	oldUser := CopilotUserURL
	CopilotUserURL = srv.URL
	t.Cleanup(func() { CopilotUserURL = oldUser })

	c := &subscriptionUsageCache
	c.Lock()
	// all four fields the call reads: asked is part of the state this test
	// primes, not something to inherit from whatever ran before it
	c.at, c.asked, c.pending = time.Now().Add(-time.Hour), false, nil
	c.data = []SubscriptionQuota{{Provider: "claude", Name: "Claude Code"}, {Provider: "codex", Name: "Codex"}}
	c.Unlock()
	t.Cleanup(func() {
		for {
			c.Lock()
			p := c.pending
			if p == nil {
				c.data, c.at = nil, time.Time{}
				c.Unlock()
				return
			}
			c.Unlock()
			<-p
		}
	})
	// before the wait above, which the held vendor would otherwise keep
	// blocked, and before the process is left with a server nobody closes
	t.Cleanup(func() { release(); srv.Close() })

	// The deadline is only so a copy that did wait fails as a FAIL rather
	// than hanging the run: nothing here is measured against it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := SubscriptionUsage(ctx)
	if len(got) != 1 || got[0].Provider != "claude" {
		t.Fatalf("got %+v, want only claude: a copy that waited for the held refresh comes back with nothing", got)
	}
	if !SubscriptionUsageReading() {
		t.Fatal("the copy came back with no refresh in flight")
	}
	select {
	case <-reached:
	case <-time.After(15 * time.Second):
		t.Fatal("a stale copy did not start a refresh")
	}
	// what showed the stale copy hears when the new one lands
	landed := make(chan struct{}, 1)
	OnSubscriptionUsage(func() {
		select {
		case landed <- struct{}{}:
		default:
		}
	})
	t.Cleanup(func() { OnSubscriptionUsage(nil) })
	release()
	c.Lock()
	p := c.pending
	c.at = time.Time{}
	c.Unlock()
	if p != nil {
		<-p
	}
	SubscriptionUsage(context.Background())
	select {
	case <-landed:
	case <-time.After(15 * time.Second):
		t.Fatal("not told the refresh landed")
	}
}

func warmSubscriptionUsageForTest(t *testing.T, cards ...SubscriptionQuota) {
	t.Helper()
	cache := &subscriptionUsageCache
	cache.Lock()
	if cache.pending != nil {
		cache.Unlock()
		t.Fatal("another usage refresh is in flight")
	}
	oldData, oldAt, oldAsked := cache.data, cache.at, cache.asked
	cache.data, cache.at, cache.asked = cards, time.Now(), false
	cache.Unlock()
	t.Cleanup(func() {
		cache.Lock()
		cache.data, cache.at, cache.asked = oldData, oldAt, oldAsked
		cache.Unlock()
	})
}

func TestSubscriptionUsageDiscoveryTimeoutKeepsSnapshot(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		return json.RawMessage(`{"windows":[{"name":"quota","used":40}]}`), nil
	}
	oldTimeout := subscriptionTimeout
	subscriptionTimeout = time.Second
	t.Cleanup(func() { subscriptionTimeout = oldTimeout })
	unlock, held := filelock.Lock(filepath.Join(host.Namespace(), "private", ".lock"), time.Second)
	if !held {
		t.Fatal("could not hold the Store lock")
	}
	release := sync.OnceFunc(unlock)
	defer release()
	synctest.Test(t, func(t *testing.T) {
		previous := SubscriptionQuota{Provider: "copilot", Name: "Copilot", User: "previous",
			Windows: []QuotaWindow{{Name: "previous window", Used: 25}}}
		warmSubscriptionUsageForTest(t, previous)
		cache := &subscriptionUsageCache
		made := time.Now().Add(-time.Hour)
		cache.Lock()
		cache.at = made
		cache.Unlock()

		pending := func() <-chan struct{} {
			cache.Lock()
			defer cache.Unlock()
			return cache.pending
		}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		SubscriptionUsage(ctx)
		if done := pending(); done != nil {
			<-done
		} else {
			t.Fatal("the stale snapshot did not start a discovery")
		}
		synctest.Wait()
		cache.Lock()
		data, at, reading := cache.data, cache.at, cache.pending != nil
		cache.Unlock()
		if reading || !at.Equal(made) || len(data) != 1 || data[0].Provider != previous.Provider ||
			len(data[0].Windows) != 1 || data[0].Windows[0].Used != 25 {
			t.Errorf("timed-out discovery changed the previous snapshot: cards=%d age=%v reading=%v",
				len(data), time.Since(at), reading)
		}

		release()
		SubscriptionUsage(t.Context())
		synctest.Wait()
		if done := pending(); done != nil {
			<-done
		}
		if cards := SubscriptionUsage(t.Context()); len(cards) != 2 || len(cards[0].Windows) != 1 || cards[0].Windows[0].Used != 40 {
			t.Errorf("discovery after release did not replace the snapshot: cards=%d", len(cards))
		}

		host.disabled = true
		Changed()
		cache.Lock()
		cache.at = time.Now().Add(-time.Hour)
		cache.Unlock()
		SubscriptionUsage(t.Context())
		if done := pending(); done != nil {
			<-done
		}
		synctest.Wait()
		cache.Lock()
		defer cache.Unlock()
		if cache.data == nil || len(cache.data) != 0 || time.Since(cache.at) >= time.Minute || cache.pending != nil {
			t.Error("completed empty discovery did not record an empty snapshot")
		}
	})
}

func TestSubscriptionUsageWarmExtensionReadReleasesCache(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		return json.RawMessage(`{"windows":[{"name":"quota","used":40}]}`), nil
	}
	logins, _ := extensionLogins(extensionAccounts()[0].ID)
	quota := extensionQuota(t.Context(), logins[0])
	if quota.Error != "" || quota.extensionUsage == nil || len(quota.Windows) != 1 {
		t.Fatalf("fixture did not read a known SDK quota: error=%q windows=%d", quota.Error, len(quota.Windows))
	}
	ordinary := SubscriptionQuota{Provider: "fixture-ordinary", Name: "Before", Windows: []QuotaWindow{{Name: "quota", Used: 27}}}
	warmSubscriptionUsageForTest(t, quota, ordinary)
	unlock, held := filelock.Lock(filepath.Join(host.Namespace(), "private", ".lock"), time.Second)
	if !held {
		t.Fatal("could not hold the Store lock")
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(unlock) }
	defer release()
	publishedName := ordinary.Name
	synctest.Test(t, func(t *testing.T) {
		call := func(deadline time.Duration) <-chan []SubscriptionQuota {
			done := make(chan []SubscriptionQuota, 1)
			go func() {
				ctx, cancel := context.WithTimeout(t.Context(), deadline)
				defer cancel()
				done <- SubscriptionUsage(ctx)
			}()
			return done
		}
		first := call(time.Second)
		synctest.Wait()
		select {
		case <-first:
			t.Fatal("the warm SDK quota did not wait on the held Store lock")
		default:
		}

		check := func(label string, cards []SubscriptionQuota, name string) {
			t.Helper()
			if len(cards) != 2 || len(cards[0].Windows) != 0 || !cards[0].ReadDeferred ||
				cards[0].Error != context.DeadlineExceeded.Error() || cards[1].Name != name ||
				len(cards[1].Windows) != 0 || !cards[1].ReadDeferred {
				t.Errorf("%s returned unusable quotas: %+v", label, cards)
			}
		}
		cache := &subscriptionUsageCache
		if cache.TryLock() {
			ordinary.Name = "Published"
			cache.data = cacheCards(cache.data, []SubscriptionQuota{ordinary}, 2, true)
			cache.Unlock()
			publishedName = ordinary.Name
			second := call(100 * time.Millisecond)
			time.Sleep(200 * time.Millisecond)
			synctest.Wait()
			select {
			case cards := <-second:
				check("another warm reader", cards, "Published")
			default:
				t.Error("another warm reader missed its deadline with the Store held")
				defer func() { <-second }()
			}
			select {
			case <-first:
				t.Error("the first warm reader returned before its deadline")
				release()
				return
			default:
			}
		} else {
			t.Error("the warm reader blocked cache publication while waiting on Store")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case cards := <-first:
			check("first warm reader", cards, "Before")
		default:
			t.Error("first warm reader missed its deadline with the Store held")
			defer func() { <-first }()
		}
		release()
	})
	if cards := SubscriptionUsage(t.Context()); len(cards) != 2 || len(cards[0].Windows) != 1 ||
		cards[0].Windows[0].Used != 40 || cards[1].Name != publishedName ||
		len(cards[1].Windows) != 1 || cards[1].Windows[0].Used != 27 {
		t.Fatalf("deadline damaged the cached snapshot: %+v", cards)
	}
}

func TestSubscriptionUsageWarmExtensionReadRevokedByDisable(t *testing.T) {
	fixture := newAccountTaskCatalogFixture(t)
	module := fixture.entry.Manifest.ID
	id := ExtensionProviderID(module, "vendor")
	logins, _ := extensionLogins(id)
	if len(logins) != 1 {
		t.Fatalf("expected one signed-in SDK account, got %d", len(logins))
	}
	quota := extensionQuota(t.Context(), logins[0])
	if quota.Error != "" || quota.extensionUsage == nil || quota.extensionUsage.ownerRevision != "" || len(quota.Windows) != 1 {
		t.Fatalf("fixture did not read a known SDK quota without a transfer: error=%q windows=%d", quota.Error, len(quota.Windows))
	}
	warmSubscriptionUsageForTest(t, quota)
	if cards := SubscriptionUsage(t.Context()); len(cards) != 1 || len(cards[0].Windows) != 1 || cards[0].Windows[0].Used != 10 {
		t.Fatalf("warm read did not show the enabled module's quota: %+v", cards)
	}
	foreign := extensionhost.NewManager(fixture.manager.Namespace())
	defer foreign.Close(t.Context())
	for _, enabled := range []bool{false, true} {
		if err := foreign.SetEnabled(t.Context(), module, enabled); err != nil {
			t.Fatal(err)
		}
		stamp := quota.extensionUsage.stamp
		if current, err := fixture.manager.Store().UsageStamp(stamp.Scope); err != nil ||
			current.Dirty || current.Revision != stamp.Revision {
			t.Fatalf("module toggle changed the fixture's usage stamp: %+v %v", current, err)
		}
		if cards := SubscriptionUsage(t.Context()); len(cards) != 1 || len(cards[0].Windows) != 0 || !cards[0].ReadDeferred {
			t.Errorf("enabled=%t: a revoked SDK quota appeared in the warm response: %+v", enabled, cards)
		}
	}
}

func TestChosenWindows(t *testing.T) {
	ws := []QuotaWindow{{Name: "Gemini 3.8 Flash (High)", Model: "gemini-3.8-flash-high"}, {Name: "Gemini 3 Flash", Model: "gemini-3-flash"}, {Name: "Weekly"}}
	got := chosenWindows(ws, map[string]bool{"gemini-3.8-flash-high": true}, nil)
	if len(got) != 2 || got[0].Model != "gemini-3.8-flash-high" || got[1].Name != "Weekly" {
		t.Fatalf("got %+v", got)
	}
	// ids the quota names that none of the enabled ones match: keep them all
	if got := chosenWindows(ws[:2], map[string]bool{"gemini-pro-agent": true}, nil); len(got) != 2 {
		t.Fatalf("kept %d of 2", len(got))
	}
	// Antigravity's levels of a model enabled as one (#150): each level's
	// window is kept for the family picked, and only those
	ag := []QuotaWindow{{Model: "gemini-3.7-flash-high"}, {Model: "gemini-3.7-flash-low"}, {Model: "gemini-3.1-pro-high"},
		{Model: "claude-opus-4-6-thinking"}, {Model: "gemini-3.5-flash-lite"}}
	base := func(id string) string {
		if b, _, ok := antigravityBaseIn(antigravityListed, id); ok {
			return b
		}
		return id
	}
	got = chosenWindows(ag, map[string]bool{"gemini-3.7-flash": true, "claude-opus-4-6-thinking": true}, base)
	if len(got) != 3 || got[0].Model != "gemini-3.7-flash-high" || got[1].Model != "gemini-3.7-flash-low" || got[2].Model != "claude-opus-4-6-thinking" {
		t.Fatalf("antigravity: %+v", got)
	}
}

// A Copilot account magpie signed in itself gets its card even when the
// editors' own sign-in isn't on the machine: every account magpie knows is
// asked, not only the one the editors' or the CLI's token belongs to.
func TestCopilotQuotaWithoutEditorsSignIn(t *testing.T) {
	home := signIn(t)
	t.Setenv("COPILOT_HOME", filepath.Join(home, ".copilot")) // not the machine's own CLI, when it has one
	// the editors' own sign-in gone: what magpie keeps is all that's left
	for _, dir := range []string{filepath.Join(home, ".config", "github-copilot"), filepath.Join(home, ".copilot")} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := copilotLogin(filepath.Join(home, ".config")); ok {
		t.Fatal("the editors' own sign-in is still readable")
	}
	if err := addCopilotLogin("hubot", "Pro+", "ghu_hubot", ""); err != nil {
		t.Fatal(err)
	}
	if ls := copilotLoginList(); len(ls) != 1 || ls[0].User != "hubot" {
		t.Fatalf("copilotLoginList = %+v", ls)
	}

	old := CopilotUserURL
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token ghu_hubot" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"copilot_plan":"individual_pro","quota_snapshots":{"premium_interactions":{"has_quota":true,"entitlement":7000,"quota_remaining":6996}}}`))
	}))
	defer srv.Close()
	CopilotUserURL = srv.URL
	t.Cleanup(func() { CopilotUserURL = old })

	var card *SubscriptionQuota
	cards, _ := fetchSubscriptionUsage(context.Background())
	for _, q := range cards {
		if q.Provider == "copilot" {
			card = &q
		}
	}
	if card == nil {
		t.Fatal("no Copilot card: an account signed in from magpie alone was left out")
	}
	if card.User != "hubot" || card.Plan != "Pro+" || len(card.Windows) != 1 {
		t.Fatalf("Copilot card = %+v", *card)
	}
	if w := card.Windows[0]; w.Name != "Premium requests" || w.Display != "4 / 7000" {
		t.Fatalf("window = %+v", w)
	}
}
