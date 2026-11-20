package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func codexPartialUsageFixture(t *testing.T) {
	t.Helper()
	home := signIn(t)
	t.Setenv("CODEX_HOME", "")
	if err := os.Remove(filepath.Join(home, ".config", "github-copilot", "apps.json")); err != nil {
		t.Fatal(err)
	}
	ForgetAccounts()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" || r.Header.Get("ChatGPT-Account-Id") != "acct-1" {
			t.Errorf("unexpected usage request: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"plan_type":"pro","rate_limit":{"allowed":true,
			"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":600},
			"secondary_window":{"used_percent":null,"limit_window_seconds":604800}}}`)
	}))
	t.Cleanup(fake.Close)
	previous := CodexBase
	CodexBase = fake.URL + "/backend-api/codex"
	t.Cleanup(func() { CodexBase = previous })
	t.Cleanup(func() {
		endReadsOut(t, "after subscription usage")
		ForgetAccounts()
	})
}

func TestCodexUsageDistinguishesZeroFromUnknown(t *testing.T) {
	codexPartialUsageFixture(t)
	ctx := context.Background()
	check := func(door string, quota SubscriptionQuota) {
		t.Helper()
		if quota.Error != "" || quota.User != "me@example.com" || quota.Plan != "pro" ||
			quota.ReadAt == nil || len(quota.Windows) != 2 {
			t.Fatalf("%s: selected-account quota = %+v", door, quota)
		}
		known, unknown := quota.Windows[0], quota.Windows[1]
		if known.Name != "5 hours" || known.Span != 5*time.Hour || known.Used != 0 ||
			known.Unknown || known.Text || known.Aside || known.ResetSecs != 600 {
			t.Errorf("%s: numeric zero lost: %+v", door, known)
		}
		if unknown.Name != "7 days" || unknown.Span != 7*24*time.Hour ||
			!unknown.Unknown || !unknown.Text || !unknown.Aside {
			t.Errorf("%s: unknown window reported as a reading: %+v", door, unknown)
		}
		if _, ok := routedAllowance("codex", quota.User, quota, time.Now()); ok {
			t.Errorf("%s: incomplete account quota became a routing allowance", door)
		}
	}
	quota := LoginUsage(ctx, "codex")["me@example.com"]
	check("LoginUsage", quota)

	for _, quota := range SubscriptionUsage(ctx) {
		if quota.Provider == "codex" {
			check("SubscriptionUsage", quota)
			return
		}
	}
	t.Fatal("SubscriptionUsage omitted the signed-in Codex account")
}

func TestCodexObservedUsageFillsUnknownWindow(t *testing.T) {
	codexPartialUsageFixture(t)
	resetUsed(t)
	ctx := context.Background()
	read := func() SubscriptionQuota {
		t.Helper()
		quota := LoginUsage(ctx, "codex")["me@example.com"]
		if len(quota.Windows) != 2 || quota.ReadAt == nil {
			t.Fatalf("expected both selected-account windows, got %+v", quota)
		}
		return quota
	}
	before := read()
	if !before.Windows[1].Unknown {
		t.Fatalf("fixture already has a measured weekly share: %+v", before.Windows[1])
	}
	NoteCodexLimits("codex", "me@example.com", []CodexLimit{{Used: 56, Span: 24 * time.Hour}})
	if window := read().Windows[1]; !window.Unknown {
		t.Fatalf("observation from a different window changed the weekly share: %+v", window)
	}
	resets := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	NoteCodexLimits("codex", "me@example.com", []CodexLimit{{Used: 0, Span: 7 * 24 * time.Hour, Resets: resets}})
	after := read()
	window := after.Windows[1]
	if window.Unknown || window.Text || window.Aside || window.noShare || window.Display != "" ||
		window.Used != 0 || window.ResetsAt == nil || !window.ResetsAt.Equal(resets) {
		t.Fatalf("measured weekly zero did not replace the unknown share: %+v", window)
	}
	if after.Windows[0].ResetSecs != before.Windows[0].ResetSecs || !before.Windows[1].Unknown ||
		!after.ReadAt.Equal(*before.ReadAt) {
		t.Fatalf("observation changed the unrelated window or read timestamp: before=%+v after=%+v", before, after)
	}
}
