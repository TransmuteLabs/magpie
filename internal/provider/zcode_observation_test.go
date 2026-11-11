package provider

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
)

func TestZCodeObservationsRemainSeparateOnQuotaReport(t *testing.T) {
	zero, used, limit := 0.0, 4.0, 10.0
	u := plugin.Usage{
		Windows: []plugin.UsageWindow{{Name: "5 hours", Used: &used}},
		Resets:  &plugin.UsageResets{Source: "legacy-list", Count: 1, ByWindow: true, Weekly: 1},
		Observations: []plugin.UsageObservation{
			{Kind: "start-balance", Name: "Trial", Remaining: &zero, Unit: "credits", Models: []string{"GLM-5.3"}, Identity: &plugin.UsageIdentity{BucketID: "bucket-1", PeriodEnd: "2030-01-01T00:00:00Z"}},
			{Kind: "server-mcp", Used: &used, Limit: &limit, Unit: "requests"},
		},
	}
	card := quotaOfPlugin(SubscriptionQuota{Provider: "zcode", User: "a", Windows: []QuotaWindow{}}, u)
	card.ZCodeReset = &ZCodeResetState{Source: "zcode", Error: "sign in again"}
	qs := quotaReport([]SubscriptionQuota{card}, nil, nil, time.Now())
	if len(qs) != 1 || len(qs[0].Windows) != 1 || len(qs[0].Observations) != 2 || qs[0].Resets.Source != "legacy-list" {
		t.Fatalf("quota report lost a separate observation: %+v", qs)
	}
	b, err := json.Marshal(qs[0])
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ZCodeReset   ZCodeResetState           `json:"zcodeReset"`
		Observations []plugin.UsageObservation `json:"observations"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.ZCodeReset.Available || out.ZCodeReset.Error == "" || out.Observations[0].Remaining == nil || *out.Observations[0].Remaining != 0 || out.Observations[0].Identity == nil || out.Observations[0].Identity.BucketID != "bucket-1" || out.Observations[1].Remaining != nil {
		t.Fatalf("native report lost unknown, zero, identity or unavailable status: %s", b)
	}
}

// The plugin reports ordinary plan windows with native and Trial exclusions;
// routing must apply them to both bare aliases and context-size variants,
// while its ordinary paid fallback remains governed by those windows.
func TestZCodeAliasQuotaProjectionDoesNotExemptPaidFallback(t *testing.T) {
	now := time.Now()
	full := 100.0
	aliases := []string{"GLM-5.3-OffPeak", "GLM-5.3-Flash-OffPeak", "GLM-5.3-Highspeed", "GLM-5.3-Flash-Highspeed", "GLM-5.3-Trial"}
	u := plugin.Usage{Windows: []plugin.UsageWindow{
		{Name: "5 hours", Used: &full, ResetsAt: now.Add(time.Hour).UTC().Format(time.RFC3339),
			Span: float64((5 * time.Hour) / time.Second), NotModels: aliases},
		{Name: "7 days", Used: &full, ResetsAt: now.Add(24 * time.Hour).UTC().Format(time.RFC3339),
			Span: float64((7 * 24 * time.Hour) / time.Second), NotModels: aliases},
		{Name: "Monthly tools", Used: &full, Aside: true, ResetsAt: now.Add(24 * time.Hour).UTC().Format(time.RFC3339)},
	}}
	q := quotaOfPlugin(SubscriptionQuota{Provider: "zcode", User: "a"}, u)
	a := allowanceOf(q.Windows, now)
	for _, model := range aliases {
		for _, id := range []string{model, model + "@1m"} {
			if got, _ := a.For(id, now); got >= 100 || !a.Full(id, 100, now).IsZero() {
				t.Errorf("native alias %s inherited the ordinary hold: %v", id, got)
			}
		}
	}
	for _, model := range []string{"GLM-5.3", "GLM-5.3-Flash", "GLM-5.3@1m", "GLM-5.1-Highspeed"} {
		if got, _ := a.For(model, now); got != 100 || a.Full(model, 100, now).IsZero() {
			t.Errorf("ordinary model or fallback %s escaped its hold: %v", model, got)
		}
	}
}
