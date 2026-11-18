package provider

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
)

func TestKeepLastUncacheableScopeSupersedesDisk(t *testing.T) {
	for _, scope := range []string{"named model", "explicit pool"} {
		t.Run(scope, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			restart := func() {
				lastQuotas.Lock()
				lastQuotas.m, lastQuotas.loaded = nil, false
				lastQuotas.Unlock()
			}
			restart()
			t.Cleanup(restart)
			now := time.Now()
			later := now.Add(time.Hour)
			const model = "gemini-4-hydrogen"
			const user = "Cache@Example.test"
			key := "ai-studio/" + strings.ToLower(user)
			legacy := SubscriptionQuota{
				Provider: "ai-studio", User: user, readSeq: 1,
				Windows: []QuotaWindow{{Name: model, Used: 100, ResetsAt: &later}},
			}
			otherAccount := legacy
			otherAccount.User = "other@example.test"
			otherProvider := legacy
			otherProvider.Provider = "other-studio"
			keepLast(legacy, "")
			keepLast(otherAccount, "")
			keepLast(otherProvider, "")
			disk := func() map[string]lastQuota {
				t.Helper()
				data, err := os.ReadFile(lastQuotasPath())
				if err != nil {
					t.Fatal(err)
				}
				var saved map[string]lastQuota
				if err := json.Unmarshal(data, &saved); err != nil {
					t.Fatal(err)
				}
				return saved
			}
			if _, ok := disk()[key]; !ok {
				t.Fatal("legacy account-wide window was not seeded on disk")
			}
			restart()
			failed := SubscriptionQuota{Provider: legacy.Provider, User: strings.ToLower(user), Error: "HTTP 429"}
			if got := keepLast(failed, ""); allowanceOf(got.Windows, now).Full("gemini-4-argon", 100, now).IsZero() {
				t.Fatal("legacy disk control did not hold the sibling model")
			}
			var usage plugin.Usage
			body := `{"windows":[{"name":"gemini-4-hydrogen","used":100,"resetsAt":"` + later.Format(time.RFC3339Nano) + `"}]}`
			if err := json.Unmarshal([]byte(body), &usage); err != nil {
				t.Fatal(err)
			}
			fresh := quotaOfPlugin(SubscriptionQuota{Provider: legacy.Provider, User: user}, usage)
			if scope == "named model" {
				fresh = namedModelWindows(fresh, []plugin.Model{{ID: model}, {ID: "gemini-4-argon"}})
			} else {
				fresh.Windows[0].matches = idSetOf([]string{model}).has
			}
			fresh.readSeq = 2
			exactScope := func(label string, got SubscriptionQuota) {
				t.Helper()
				if got.Error != "" || len(got.Windows) != 1 {
					t.Fatalf("%s: fresh scoped reading was lost: error=%q windows=%d", label, got.Error, len(got.Windows))
				}
				allowance := allowanceOf(got.Windows, now)
				if allowance.Full(model, 100, now).IsZero() {
					t.Errorf("%s: the named model lost its cooldown", label)
				}
				for _, sibling := range []string{"gemini-4-argon", model + "-flash"} {
					if until := allowance.Full(sibling, 100, now); !until.IsZero() {
						t.Errorf("%s: exact-scoped cooldown holds sibling %s until %v", label, sibling, until)
					}
				}
			}
			exactScope("fresh", keepLast(fresh, ""))
			exactScope("transient failure", keepLast(failed, ""))
			exactScope("late older response", keepLast(legacy, ""))
			keepLast(otherAccount, "")
			saved := disk()
			if _, ok := saved[key]; ok {
				t.Error("obsolete or unserializable same-account reading remains on disk")
			}
			for _, kept := range []string{"ai-studio/" + strings.ToLower(otherAccount.User), "other-studio/" + strings.ToLower(user)} {
				if _, ok := saved[kept]; !ok {
					t.Errorf("unrelated cached account/provider %s was evicted", kept)
				}
			}
			restart()
			got := keepLast(failed, "")
			if got.Error != failed.Error || got.AsOf != nil || len(got.Windows) != 0 {
				t.Errorf("reload resurrected obsolete account-wide fallback: error=%q windows=%d asOf=%v", got.Error, len(got.Windows), got.AsOf)
			}
			if got := keepLast(SubscriptionQuota{Provider: otherAccount.Provider, User: otherAccount.User, Error: "HTTP 429"}, ""); got.Error != "" || got.AsOf == nil || len(got.Windows) != 1 {
				t.Error("reload lost unrelated serializable account fallback")
			}
		})
	}
}
