package gui

import (
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestTrayCardFreshnessText(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.Local)
	read := now.Add(-3 * time.Hour)
	for _, lang := range []struct {
		code, failed, deferred, deferredAt string
	}{
		{"en", "as of Sep 27 09:00, couldn't be read just now", "not refreshed this time", "as of Sep 27 09:00, not refreshed this time"},
		{"zh", "截至 9月27日 09:00，暂时无法获取最新额度", "本次未刷新", "截至 9月27日 09:00，本次未刷新"},
		{"zh-TW", "截至 9月27日 09:00，暫時無法取得最新額度", "本次未重新整理", "截至 9月27日 09:00，本次未重新整理"},
		{"ja", "9月27日 09:00 時点、現在は読み取れません", "今回は更新していません", "9月27日 09:00 時点、今回は更新していません"},
		{"de", "Stand 27.9. 09:00, konnte gerade nicht gelesen werden", "diesmal nicht aktualisiert", "Stand 27.9. 09:00, diesmal nicht aktualisiert"},
	} {
		t.Run(lang.code, func(t *testing.T) {
			for _, state := range []struct {
				name     string
				asOf     *time.Time
				deferred bool
				want     string
			}{
				{name: "fresh"},
				{name: "failed", asOf: &read, want: "\n" + lang.failed},
				{name: "deferred", deferred: true, want: "\n" + lang.deferred},
				{name: "deferred-as-of", asOf: &read, deferred: true, want: "\n" + lang.deferredAt},
			} {
				t.Run(state.name, func(t *testing.T) {
					for _, card := range []struct {
						name, label string
						quota       provider.SubscriptionQuota
					}{
						{name: "error", quota: provider.SubscriptionQuota{Error: "unavailable", Windows: []provider.QuotaWindow{{Name: "Window", Used: 25}}}},
						{name: "balance", label: "$4.20", quota: provider.SubscriptionQuota{Balance: "$4.20"}},
						{name: "window", label: "25%", quota: provider.SubscriptionQuota{Windows: []provider.QuotaWindow{{Name: "Window", Used: 25}}}},
					} {
						t.Run(card.name, func(t *testing.T) {
							quota := card.quota
							quota.Name, quota.AsOf, quota.ReadDeferred = "Sample", state.asOf, state.deferred
							label, tip := trayCardText(lang.code, quota, now, false, false, nil)
							if label != card.label {
								t.Fatalf("label %q, want %q", label, card.label)
							}
							if got := trayAsOf(lang.code, quota); got != state.want {
								t.Errorf("freshness %q, want %q", got, state.want)
							}
							if !strings.HasSuffix(tip, state.want) {
								t.Errorf("tooltip %q lacks freshness %q", tip, state.want)
							}
							if quota.Error != "" && tip != "Sample: unavailable"+state.want {
								t.Errorf("error tooltip %q", tip)
							}
						})
					}
				})
			}
		})
	}
}

func TestTrayUsageText(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	in := func(d time.Duration) *time.Time { at := now.Add(d); return &at }

	// the shortest window first, whatever order the vendor gives them in;
	// on-demand spending and a single model's window are left out
	q := provider.SubscriptionQuota{Provider: "claude", Name: "Claude", User: "a@b.c", Windows: []provider.QuotaWindow{
		{Name: "Weekly", Used: 17.6, ResetsAt: in(3*24*time.Hour + 4*time.Hour), Span: 7 * 24 * time.Hour},
		{Name: "5-hour", Used: 42.2, ResetsAt: in(2*time.Hour + 10*time.Minute), Span: 5 * time.Hour},
		{Name: "Opus weekly", Used: 90, Model: "opus", Span: 7 * 24 * time.Hour},
		{Name: "Extra usage", Used: 5, Aside: true},
	}}
	label, tip := trayUsageText(q, now, false, false)
	if label != "42% · 18%" {
		t.Errorf("label %q", label)
	}
	if want := "Claude\n5-hour 42.2% used · resets in 2h 10m\nWeekly 17.6% used · resets in 3d 4h"; tip != want {
		t.Errorf("tip %q, want %q", tip, want)
	}
	// or what is left of each, as Settings or the Usage page says (#122)
	label, tip = trayUsageText(q, now, true, false)
	if label != "58% · 82%" {
		t.Errorf("left label %q", label)
	}
	if want := "Claude\n5-hour 57.8% left · resets in 2h 10m\nWeekly 82.4% left · resets in 3d 4h"; tip != want {
		t.Errorf("left tip %q, want %q", tip, want)
	}
	if id := trayCardID(q); id != "claude|a@b.c" {
		t.Errorf("id %q", id)
	}

	// no lengths known: the vendor's order, two at most, clamped, its own count
	q = provider.SubscriptionQuota{Provider: "zcode", Name: "ZCode", Windows: []provider.QuotaWindow{
		{Name: "5 小时", Used: 120, Display: "1.2k / 1k"},
		{Name: "Weekly", Used: -3, ResetSecs: 90},
		{Name: "Monthly", Used: 1},
	}}
	label, tip = trayUsageText(q, now, false, false)
	if label != "100% · 0%" {
		t.Errorf("label %q", label)
	}
	if want := "ZCode\n5 小时 1.2k / 1k · 100% used\nWeekly 0% used · resets in 2m\nMonthly 1% used"; tip != want {
		t.Errorf("tip %q, want %q", tip, want)
	}
	if id := trayCardID(q); id != "zcode" {
		t.Errorf("id %q", id)
	}

	q = provider.SubscriptionQuota{Name: "Copilot", Windows: []provider.QuotaWindow{{Name: "Premium", Used: 187418.3 / 218000 * 100}}}
	label, tip = trayUsageText(q, now, false, false)
	if label != "86%" || tip != "Copilot\nPremium 86.0% used" {
		t.Errorf("copilot used: %q %q", label, tip)
	}
	label, tip = trayUsageText(q, now, true, false)
	if label != "14%" || tip != "Copilot\nPremium 14.0% left" {
		t.Errorf("copilot left: %q %q", label, tip)
	}

	// a balance, an error, nothing
	if label, _ = trayUsageText(provider.SubscriptionQuota{Name: "DeepSeek", Balance: "¥12.30"}, now, false, false); label != "¥12.30" {
		t.Errorf("balance label %q", label)
	}
	// one read a while ago, standing in for one that couldn't be read now,
	// says when it was read (#420)
	read := time.Date(2026, 9, 27, 9, 5, 0, 0, time.Local)
	if label, tip = trayUsageText(provider.SubscriptionQuota{Name: "Relay", Balance: "$4.20", AsOf: &read}, now, false, false); label != "$4.20" || tip != "Relay · $4.20\nas of Sep 27 09:05, couldn't be read just now" {
		t.Errorf("balance as of: %q %q", label, tip)
	}
	if _, tip = trayUsageText(provider.SubscriptionQuota{Name: "Kimi", AsOf: &read, Windows: []provider.QuotaWindow{{Name: "Weekly", Used: 10}}}, now, false, false); tip != "Kimi\nWeekly 10% used\nas of Sep 27 09:05, couldn't be read just now" {
		t.Errorf("windows as of: %q", tip)
	}
	if label, tip = trayUsageText(provider.SubscriptionQuota{Name: "Codex", Error: "signed out"}, now, false, false); label != "" || tip != "Codex: signed out" {
		t.Errorf("error: %q %q", label, tip)
	}
	if label, tip = trayUsageText(provider.SubscriptionQuota{Name: "Empty"}, now, false, false); label != "" || tip != "" {
		t.Errorf("empty: %q %q", label, tip)
	}
}
