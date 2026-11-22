package provider

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
)

func TestKeptSDKUsageBatchesCacheKeyReads(t *testing.T) {
	fixture := keepAliveUsageFixture(t, "synthetic-allowed")
	store := fixture.manager.Store()
	id := ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider)
	lockPath := filepath.Join(fixture.manager.Namespace(), "private", ".lock")

	loginUsageCache.Lock()
	previousReadings := loginUsageCache.m
	loginUsageCache.Unlock()
	previousReckoner := ReckonHolds
	ReckonHolds = nil
	t.Cleanup(func() {
		loginUsageCache.Lock()
		loginUsageCache.m = previousReadings
		loginUsageCache.Unlock()
		ReckonHolds = previousReckoner
	})

	saved, singleReads := 1, 0
	for _, count := range []int{1, 10, 100} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			for saved < count {
				if _, err := store.SaveAccount(extensionhost.Account{
					Plugin: fixture.scope.Plugin, Provider: fixture.scope.Provider, ID: "account-" + strconv.Itoa(saved),
					Auth: json.RawMessage(`{"token":"synthetic-allowed"}`),
				}, 0); err != nil {
					t.Fatal(err)
				}
				saved++
			}

			logins, _ := extensionLoginsContext(t.Context(), id)
			if len(logins) != count {
				t.Fatalf("discovered %d SDK accounts, want %d", len(logins), count)
			}
			var page []SubscriptionQuota
			readings := make(map[string]loginUsageEntry, count)
			users := make(map[string]bool, count)
			now := time.Now()
			for _, login := range logins {
				if users[login.User] {
					t.Fatalf("discovered duplicate SDK account %q", login.User)
				}
				users[login.User] = true
				_, proof, err := extensionUsageRequest(t.Context(), login.extension)
				if err != nil {
					t.Fatal(err)
				}
				card := SubscriptionQuota{Provider: id, User: login.User, Name: "Fixture",
					Windows: []QuotaWindow{{Name: "quota", Used: 20}}, readSeq: 1, extensionUsage: proof}
				page = append(page, card)
				card.Windows, card.readSeq = []QuotaWindow{{Name: "quota", Used: 40}}, 2
				readings[loginReadingCacheKeyContext(t.Context(), login)] = loginUsageEntry{at: now, q: card, read: card}
			}
			loginUsageCache.Lock()
			loginUsageCache.m = readings
			loginUsageCache.Unlock()
			warmSubscriptionUsageForTest(t, page...)

			check := func(report []Quota, revoked string) {
				t.Helper()
				seen := make(map[string]bool, count)
				for _, quota := range report {
					if quota.Provider != id {
						continue
					}
					if !users[quota.User] || seen[quota.User] {
						t.Fatalf("kept report published an unexpected or duplicate SDK account %q", quota.User)
					}
					seen[quota.User] = true
					if quota.User == revoked {
						if !quota.ReadDeferred || len(quota.Windows) != 0 {
							t.Fatalf("kept report published revoked windows: %+v", quota)
						}
					} else if quota.ReadDeferred || len(quota.Windows) != 1 || quota.Windows[0].Used != 40 {
						t.Fatalf("kept report did not select the current cached reading: %+v", quota)
					}
				}
				if len(seen) != count {
					t.Fatalf("published %d distinct SDK quotas, want %d", len(seen), count)
				}
			}
			check(KeptQuotaReportContext(t.Context(), now), "")
			reads := countProviderCatalogReads(t, lockPath, func() {
				check(KeptQuotaReportContext(t.Context(), now), "")
			})
			t.Logf("ordinary kept quota for %d SDK accounts acquired %d private Store reads", count, reads)
			if count == 1 {
				singleReads = reads
				if singleReads == 0 {
					t.Fatal("observer did not see the Store reads")
				}
			} else if reads != singleReads {
				t.Errorf("kept quota read the private Store %d times for %d accounts; want %d as for one account", reads, count, singleReads)
			}
			if count == 100 {
				ReckonHolds = func(ctx context.Context, cards []SubscriptionQuota, _ time.Time) []SubscriptionQuota {
					prior := cardOf(cards, id, page[0].User)
					if prior == nil || len(prior.Windows) != 1 || prior.Windows[0].Used != 40 {
						t.Fatalf("kept reading was absent before the final proof: %+v", prior)
					}
					raw, err := json.Marshal(page[0].extensionUsage.stamp.Scope)
					if err != nil {
						t.Fatal(err)
					}
					command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExtensionUsageChild$")
					command.Env = append(os.Environ(), "SDK_USAGE_CHILD_ROOT="+fixture.manager.Namespace(),
						"SDK_USAGE_CHILD_SCOPE="+string(raw))
					if output, err := command.CombinedOutput(); err != nil {
						t.Fatalf("peer usage invalidation: %v\n%s", err, output)
					}
					return cards
				}
				for range 2 {
					report := KeptQuotaReportContext(t.Context(), now)
					ReckonHolds = nil
					wire, err := json.Marshal(report)
					if err != nil {
						t.Fatal(err)
					}
					var decoded []Quota
					if err := json.Unmarshal(wire, &decoded); err != nil {
						t.Fatal(err)
					}
					check(decoded, page[0].User)
				}
			}
			if fixture.manager.Running(fixture.scope.Plugin) || fixture.calls.Load() != 0 {
				t.Fatal("reading warm quota cards started the SDK worker or asked the vendor")
			}
		})
	}
}
