package gateway

import (
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// Kiro spends an account's plan credits first, then its trial's, and the
// plan's end at the month's reset while a trial's run a year. Its plugin
// says a month used up with a trial left goes on on credits
// (plugin.UsageWindow.Credits): that account is asked after every account
// of the provider with plan credits left, under any routing but the
// user's pick, and before those with nothing left; the trace says why.
// Before, the plugin set the month aside, and an account at 100% of its
// month spending its trial was asked first in its listed place while
// eleven accounts' plan credits went unspent toward their reset.
func TestPluginCreditsAfterTheOthers(t *testing.T) {
	fresh(t)
	forgetRouting()
	now := time.Now()
	reset := now.Add(12 * 24 * time.Hour).UTC().Format(time.RFC3339)
	month := func(used float64, credits bool) string {
		w := `{"name":"Credits","used":` + ftoa(used) + `,"amount":` + ftoa(used*9.16) + `,"limit":916,"unit":"credits","span":2592000,"resetsAt":"` + reset + `"`
		if credits {
			w += `,"aside":true,"credits":true`
		}
		return w + `}`
	}
	trial := func(used float64) string {
		return `{"name":"Trial credits","used":` + ftoa(used) + `,"amount":` + ftoa(used*5) + `,"limit":500,"unit":"credits","aside":true}`
	}
	usage := func(ws ...string) provider.Allowance {
		b := `{"windows":[`
		for i, w := range ws {
			if i > 0 {
				b += ","
			}
			b += w
		}
		return provider.PluginAllowanceForTest([]byte(b+`]}`), now)
	}
	al := map[string]provider.Allowance{
		"trial@kiro": usage(month(100, true), trial(80.84)), // its month spent, on its trial
		"spent@kiro": usage(month(100, false), trial(100)),  // nothing left
		"plan@kiro":  usage(month(0, false), trial(0)),      // its month untouched
		"half@kiro":  usage(month(48, false), trial(0)),
	}
	old := allowances
	defer func() { allowances = old }()
	reads := 0
	allowances = func(agent string) map[string]provider.Allowance {
		if agent != "plugin:kiro" {
			return nil
		}
		reads++
		return al
	}
	acct := func(u string) candidate {
		return candidate{p: provider.Provider{ID: "kiro", Account: &provider.Account{Agent: "plugin:kiro", User: u}}, model: "claude-sonnet-4.5", rest: "kiro#" + u}
	}
	if a := al["trial@kiro"]; !a.OnCredits() {
		t.Fatalf("a month spent with credits to go on on is not on them: %+v", a)
	}
	if c := acct("trial@kiro"); !c.full(now).IsZero() {
		t.Fatal("an account on its trial is taken for used up")
	}
	refusal := []byte(`{"error":{"message":"usage limit reached for your plan"}}`)
	trialAccount := acct("trial@kiro")
	if creditsAgain(trialAccount, http.StatusTooManyRequests, refusal) {
		t.Error("trial credits enabled the immediate ChatGPT quota retry")
	}
	rest := new(Server).restAfter(trialAccount, http.StatusTooManyRequests, http.Header{"Retry-After": {"1800"}}, refusal)
	if rest.By != "retry-after" || time.Until(rest.Until) < 29*time.Minute {
		t.Errorf("trial refusal lost the vendor's wait: %+v", rest)
	}
	forgetRouting()
	order := func(routing string, users ...string) []string {
		t.Helper()
		var cs []candidate
		for _, u := range users {
			cs = append(cs, acct(u))
		}
		p := provider.Provider{ID: "kiro", Routing: routing}
		before := reads
		out, wg := weigh(p, cs, "claude-sonnet-4.5", provider.Chat)
		if reads != before+1 {
			t.Fatalf("%q: weigh read the allowance owner %d times, want one snapshot", routing, reads-before)
		}
		var got []string
		for _, c := range out {
			got = append(got, c.p.Account.User)
		}
		if routing != provider.Manual {
			w := weighed(acct("trial@kiro"), p, wg, false, provider.Chat)
			if !w.Credits || !w.CreditsLast {
				t.Fatalf("%q: the trial's account is told %+v", routing, w)
			}
		}
		return got
	}
	for _, routing := range []string{"", provider.Ordered, provider.Drain, provider.FreeFirst} {
		got := order(routing, "trial@kiro", "spent@kiro", "plan@kiro", "half@kiro")
		if i := slices.Index(got, "trial@kiro"); i != 2 || got[3] != "spent@kiro" {
			t.Fatalf("%q: order %v, want the two with plan credits first, then the trial's, then the spent", routing, got)
		}
	}
	// with no plan credits left anywhere the trial is spent first
	if got := order(provider.Ordered, "spent@kiro", "trial@kiro"); got[0] != "trial@kiro" {
		t.Fatalf("order %v, want the trial's before the spent", got)
	}
	// the account the user picked stays where they put it
	if got := order(provider.Manual, "trial@kiro", "plan@kiro"); got[0] != "trial@kiro" {
		t.Fatalf("manual order %v", got)
	}
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func TestWeighDoesNotRereadAllowancesWithoutTrialCredits(t *testing.T) {
	fresh(t)
	forgetRouting()
	for _, agent := range []string{provider.ExtensionProviderID("devin-inbound", "devin"), "plugin:kiro"} {
		t.Run(agent, func(t *testing.T) {
			old := allowances
			t.Cleanup(func() { allowances = old })
			reads := 0
			now := time.Now()
			allowances = func(id string) map[string]provider.Allowance {
				if id != agent {
					t.Fatalf("unexpected allowance owner %q", id)
				}
				reads++
				return map[string]provider.Allowance{
					"first":  {{Used: 25, Resets: now.Add(time.Hour)}},
					"second": {{Used: 50, Resets: now.Add(time.Hour)}},
				}
			}
			pool := provider.Provider{ID: "example", Routing: provider.Ordered}
			var candidates []candidate
			for _, user := range []string{"first", "second"} {
				picked := pool
				picked.Account = &provider.Account{Agent: agent, User: user}
				candidates = append(candidates, candidate{p: picked, model: "m", rest: pool.ID + "#" + user})
			}
			ordered, weighed := weigh(pool, candidates, "m", provider.Chat)
			if len(ordered) != 2 || ordered[0].p.Account.User != "first" || len(weighed.behind) != 0 {
				t.Fatalf("ordinary quotas changed listed order: %+v, %+v", ordered, weighed.behind)
			}
			if reads != 1 {
				t.Fatalf("weigh read the same allowance owner %d times, want one snapshot", reads)
			}
		})
	}
}
