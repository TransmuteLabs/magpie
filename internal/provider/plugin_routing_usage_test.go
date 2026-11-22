package provider

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
)

func TestPluginRoutingFallbackRejectsUnreadableLogins(t *testing.T) {
	login, requests := startPluginUsageFixture(t)
	startWithNoAllowancesRead(t)
	t.Cleanup(func() { endReadsOut(t, "after the plugin routing fixture") })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	releaseWait := Unwaited()
	defer releaseWait()
	Allowances(login.Agent)
	select {
	case reply := <-requests:
		reply <- pluginUsageReply{http.StatusOK, `{"signIn":"kept","windows":[{"name":"5 hours","used":80,"resetSecs":3600}]}`}
	case <-ctx.Done():
		t.Fatal("the plugin did not receive an allowance read", ctx.Err())
	}
	endReadsOut(t, "after keeping the first allowance")
	if before := lastAllowances(login.Agent)[login.User]; len(before) != 1 || before[0].Used != 80 {
		t.Fatalf("fixture did not keep the allowance: %+v", before)
	}
	if err := os.WriteFile(loginsPath(), []byte(`{"interrupted`), 0o600); err != nil {
		t.Fatal(err)
	}
	if after := lastAllowances(login.Agent)[login.User]; len(after) != 0 {
		t.Fatalf("routing used the saved allowance without a readable sign-in: %+v", after)
	}
}

func TestPluginRoutingUsageAfterPeerSignIn(t *testing.T) {
	const child = "MAGPIE_TEST_PLUGIN_ROUTING_SIGNIN"
	if os.Getenv(child) != "" {
		t.Cleanup(stopPluginHost)
		t.Cleanup(plugin.Settle)
		ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
		defer cancel()
		slot, err := plugin.Import(ctx, "fakeco", map[string]any{
			"type": "oauth", "refresh": "replacement", "access": "replacement", "expires": 9e15, "accountId": "usage@fake",
		})
		if err != nil {
			t.Fatal(err)
		}
		PluginSignedIn(plugin.Saved{Provider: "fakeco", Account: slot})
		return
	}

	for _, scenario := range []struct {
		name     string
		alias    bool
		inFlight bool
	}{
		{name: "warm-plugin"},
		{name: "warm-public-id", alias: true},
		{name: "late-public-id", alias: true, inFlight: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			login, requests := startPluginUsageFixture(t)
			startWithNoAllowancesRead(t)
			t.Cleanup(func() { endReadsOut(t, "after the plugin routing fixture") })
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			releaseWait := Unwaited()
			defer releaseWait()
			agent := login.Agent
			if scenario.alias {
				agent = PluginID("fakeco")
			}
			const sibling = "sibling@fake"
			siblingSlot, err := plugin.Import(ctx, "fakeco", map[string]any{
				"type": "oauth", "refresh": "sibling", "access": "sibling", "expires": 9e15, "accountId": sibling,
			})
			if err != nil {
				t.Fatal(err)
			}
			saveLogins(t, append(readLogins(), savedLogin{Agent: login.Agent, User: sibling, Home: siblingSlot, On: true})...)
			if logins, _ := usageLogins(agent); len(logins) != 2 {
				t.Fatalf("fixture needs two routable logins, got %+v", logins)
			}

			sendQuota := func(used int) {
				t.Helper()
				select {
				case reply := <-requests:
					reply <- pluginUsageReply{http.StatusOK, fmt.Sprintf(`{"signIn":"kept","windows":[{"name":"5 hours","used":%d,"resetSecs":3600}]}`, used)}
				case <-ctx.Done():
					t.Fatal("the plugin did not receive an allowance read", ctx.Err())
				}
			}
			checkQuota := func(allowances map[string]Allowance, user string, used float64) {
				t.Helper()
				if limits := allowances[user]; len(limits) != 1 || limits[0].Used != used {
					t.Errorf("allowance for %s = %+v, want %g%%", user, limits, used)
				}
			}

			Allowances(agent)
			sendQuota(80)
			sendQuota(80)
			endReadsOut(t, "after the initial allowance")
			original := Allowances(agent)
			checkQuota(original, login.User, 80)
			checkQuota(original, sibling, 80)
			changedAt, failed, known := UsageRead(agent, login.User)
			if !known || failed || changedAt.IsZero() {
				t.Fatalf("initial UsageRead = %v, failed %t, known %t", changedAt, failed, known)
			}
			siblingAt, failed, known := UsageRead(agent, sibling)
			if !known || failed || siblingAt.IsZero() {
				t.Fatalf("initial sibling UsageRead = %v, failed %t, known %t", siblingAt, failed, known)
			}

			entered, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			if scenario.inFlight {
				history := noteHistoryWrite
				noteHistoryWrite = func(context.Context, []SubscriptionQuota, time.Time) {
					select {
					case <-entered:
					default:
						close(entered)
					}
					<-release
				}
				t.Cleanup(func() {
					unblock()
					endReadsOut(t, "before restoring the quota history hook")
					noteHistoryWrite = history
				})
				StaleAllowance(login.Agent, login.User)
				if agent != login.Agent {
					StaleAllowance(agent, login.User)
				}
				Allowances(agent)
				sendQuota(95)
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("the old reading did not reach the publication boundary", ctx.Err())
				}
			}

			before := loginOf(t, readLogins(), login.Agent, login.User)
			peer := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestPluginRoutingUsageAfterPeerSignIn$", "-test.count=1")
			peer.Env = append(os.Environ(), child+"=1")
			if out, err := peer.CombinedOutput(); err != nil {
				t.Fatalf("peer sign-in: %v\n%s", err, out)
			}
			after := loginOf(t, readLogins(), login.Agent, login.User)
			if before.Home != after.Home || loginID(before) != loginID(after) || after.PluginObservations.Epoch == "" ||
				before.PluginObservations.Epoch == after.PluginObservations.Epoch {
				t.Fatalf("peer sign-in did not replace the lifetime of the same login: %+v → %+v", before.PluginObservations, after.PluginObservations)
			}

			if scenario.inFlight {
				unblock()
				endReadsOut(t, "after the replaced reading completed")
				usedCache.Lock()
				old := usedCache.m[agent][login.User]
				usedCache.Unlock()
				if len(old) != 0 {
					t.Errorf("the late reply published an allowance for the replaced login: %+v", old)
				}
			}
			var selected map[string]Allowance
			if scenario.alias && !scenario.inFlight {
				selected = Allowances(agent)
			}
			if at, failed, known := UsageRead(agent, login.User); known || failed || !at.IsZero() {
				t.Errorf("the replaced login kept its former UsageRead: %v, failed %t, known %t", at, failed, known)
			}
			if at, failed, known := UsageRead(agent, sibling); !known || failed || !at.Equal(siblingAt) {
				t.Errorf("the unchanged sibling lost its UsageRead: %v, failed %t, known %t", at, failed, known)
			}
			if selected == nil {
				select {
				case reply := <-requests:
					reply <- pluginUsageReply{http.StatusServiceUnavailable, "{}"}
					t.Error("UsageRead started a vendor read")
				default:
				}
				selected = Allowances(agent)
			}
			if old := selected[login.User]; len(old) != 0 {
				t.Errorf("routing used the former allowance for a replaced login: %+v", old)
			}
			checkQuota(selected, sibling, 80)
			checkQuota(original, login.User, 80)
			checkQuota(original, sibling, 80)
			last := lastAllowances(agent)
			if old := last[login.User]; len(old) != 0 {
				t.Errorf("the cold fallback accepted the former allowance: %+v", old)
			}
			checkQuota(last, sibling, 80)

			if !t.Failed() {
				sendQuota(25)
				endReadsOut(t, "after reading the replacement")
				checkQuota(Allowances(agent), login.User, 25)
			}
		})
	}
}
