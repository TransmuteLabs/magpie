package provider

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

func TestQuotaHistoryChecksProofAfterWriterWait(t *testing.T) {
	host, used, calls := prepareUsageReadFixture(t)
	providerID := ExtensionProviderID(host.manifest.ID, "vendor")
	logins, ok := extensionLoginsContext(t.Context(), providerID)
	if !ok || len(logins) != 2 || logins[0].User != "alice" || logins[1].User != "bob" {
		t.Fatal("both fixture logins must be available")
	}
	at := time.Now().UTC().Truncate(time.Second)
	later := at.Add(15 * time.Minute)
	read := func(login Login, at time.Time) SubscriptionQuota {
		t.Helper()
		card := extensionQuota(t.Context(), login)
		if card.Error != "" || len(card.Windows) != 1 || !card.extensionUsage.currentContext(t.Context()) {
			t.Fatalf("fixture reading for %s: error=%q windows=%d", login.User, card.Error, len(card.Windows))
		}
		card.ReadAt = &at
		return card
	}
	first := read(logins[0], at)
	noteQuotaHistoryContext(t.Context(), []SubscriptionQuota{first}, at)
	used.Store(35)
	stale, sibling := read(logins[0], later), read(logins[1], later)
	before := calls.Load()

	changeWhileHistoryWaits(t, &filelock.Mutex{Path: quotaHistMu.Path}, func(ctx context.Context) {
		noteQuotaHistoryContext(ctx, []SubscriptionQuota{stale, sibling}, later)
	}, func(t *testing.T) {
		if _, err := host.store.InvalidateUsage(stale.extensionUsage.stamp.Scope); err != nil {
			t.Fatal(err)
		}
		if stale.extensionUsage.currentContext(t.Context()) || !sibling.extensionUsage.currentContext(t.Context()) {
			t.Fatal("fixture must revoke only the first account's reading")
		}
	})

	history := QuotaHistories(at, providerID, "")
	if points := lineOf(t, history, "alice", "quota"); len(points) != 1 || !points[0].At.Equal(at) || points[0].Left != 80 {
		t.Errorf("revoked quota changed the previous points: %+v", points)
	}
	if got := lastNoted[quotaHistKey(providerID, "alice")]; !got.Equal(at) {
		t.Errorf("revoked quota advanced the last note to %v", got)
	}
	if points := lineOf(t, history, "bob", "quota"); len(points) != 1 || !points[0].At.Equal(later) || points[0].Left != 65 {
		t.Errorf("current sibling quota was not recorded: %+v", points)
	}
	if got := calls.Load(); got != before {
		t.Errorf("history made %d additional vendor calls", got-before)
	}
}

func TestCardHistoryChecksLegacyOwnerAfterWriterWait(t *testing.T) {
	for _, name := range []string{"quota", "login-quota", "balance"} {
		t.Run(name, func(t *testing.T) {
			host := fixtureExtensionProvider(t, true)
			const ownerID = "transferred-subscription"
			const siblingID = "other-subscription"
			at := time.Now().UTC().Truncate(time.Second)
			later := at.Add(15 * time.Minute)
			first := SubscriptionQuota{Provider: ownerID, User: "alice"}
			var next SubscriptionQuota
			var mutex sync.Locker
			write := noteCardHistory
			switch name {
			case "quota", "login-quota":
				first.Windows = []QuotaWindow{{Name: "Weekly", Used: 20}}
				next = SubscriptionQuota{Provider: ownerID, User: "alice", Windows: []QuotaWindow{{Name: "Weekly", Used: 35}}}
				mutex = &filelock.Mutex{Path: quotaHistMu.Path}
				if name == "login-quota" {
					write = noteHistoryWrite
				}
			case "balance":
				first.Balance = "$20"
				next = SubscriptionQuota{Provider: ownerID, User: "alice", Balance: "$15"}
				mutex = &filelock.Mutex{Path: balanceHistMu.Path}
			}
			sibling := next
			sibling.Provider = siblingID
			write(t.Context(), []SubscriptionQuota{first}, at)
			cards := []SubscriptionQuota{next, sibling}
			changeWhileHistoryWaits(t, mutex, func(ctx context.Context) {
				write(ctx, cards, later)
			}, func(t *testing.T) {
				usageTestSubscriptionOwner(t, host, ownerID)
				if subscriptionQuotaCheck(t.Context())(next) {
					t.Fatal("fixture did not publish a new subscription owner")
				}
			})
			switch name {
			case "quota", "login-quota":
				if name == "quota" {
					if !cards[0].ReadDeferred || len(cards[0].Windows) != 0 {
						t.Errorf("the revoked reading could reach the cache: %+v", cards[0])
					}
					if cards[1].ReadDeferred || len(cards[1].Windows) != 1 || cards[1].Windows[0].Used != 35 {
						t.Errorf("the unaffected reading was lost: %+v", cards[1])
					}
				}
				history := QuotaHistories(at, "", "alice")
				if points := lineOf(t, QuotaHistories(at, ownerID, ""), "alice", "Weekly"); len(points) != 1 || !points[0].At.Equal(at) || points[0].Left != 80 {
					t.Errorf("old owner's quota changed the previous points: %+v", points)
				}
				if got := lastNoted[quotaHistKey(ownerID, "alice")]; !got.Equal(at) {
					t.Errorf("old owner's quota advanced the last note to %v", got)
				}
				if points := lineOf(t, QuotaHistories(at, siblingID, ""), "alice", "Weekly"); len(history) != 2 || len(points) != 1 || !points[0].At.Equal(later) || points[0].Left != 65 {
					t.Errorf("current sibling quota was not recorded: %+v", history)
				}
				plan := next
				plan.User = "separate-key"
				noteQuotaHistoryContext(t.Context(), []SubscriptionQuota{plan}, later)
				if points := lineOf(t, QuotaHistories(at, ownerID, plan.User), plan.User, "Weekly"); len(points) != 1 || points[0].Left != 65 {
					t.Errorf("subscription ownership blocked the key plan's quota: %+v", points)
				}
			case "balance":
				history := readBalanceHist()
				if points := history[quotaHistKey(ownerID, "alice")]; len(points) != 1 || !points[0].At.Equal(at) || points[0].Amount != 20 {
					t.Errorf("old owner's balance changed the previous points: %+v", points)
				}
				if cards[0].BalanceTrend != nil {
					t.Errorf("old owner's card received a trend: %+v", cards[0].BalanceTrend)
				}
				if points := history[quotaHistKey(siblingID, "alice")]; len(points) != 1 || !points[0].At.Equal(later) || points[0].Amount != 15 {
					t.Errorf("current sibling balance was not recorded: %+v", points)
				}
				noteBalanceHistory(t.Context(), []SubscriptionQuota{{Provider: ownerID, User: "separate-key", Balance: "$9"}}, later)
				if points := readBalanceHist()[quotaHistKey(ownerID, "separate-key")]; len(points) != 1 || points[0].Amount != 9 {
					t.Errorf("subscription ownership blocked the ordinary key balance: %+v", points)
				}
			}
		})
	}
}

func TestDailyCreditsChecksLegacyOwnerAfterWriterWait(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	const ownerID = "transferred-subscription"
	const siblingID = "other-subscription"
	at := time.Now().UTC().Truncate(time.Second)
	later := at.Add(15 * time.Minute)
	noteCardHistory(t.Context(), []SubscriptionQuota{
		wbCard(ownerID, "alice", 20, 100),
		wbCard(siblingID, "bob", 20, 100),
	}, at)
	cards := []SubscriptionQuota{
		wbCard(ownerID, "alice", 35, 100),
		wbCard(siblingID, "bob", 35, 100),
	}
	observed := slices.Clone(cards)
	changeWhileHistoryWaits(t, &creditsMu, func(ctx context.Context) {
		noteCardHistory(ctx, cards, later)
	}, func(t *testing.T) {
		usageTestSubscriptionOwner(t, host, ownerID)
	})
	if !cards[0].ReadDeferred || len(cards[0].Windows) != 0 {
		t.Errorf("the former owner's card was not deferred: %+v", cards[0])
	}
	history := withDailyCredits(observed, later)
	if daily := dailyOf(t, history, "alice"); daily == nil || len(daily.Days) != 0 {
		t.Errorf("old owner's credits changed: %+v", daily)
	}
	if daily := dailyOf(t, history, "bob"); daily == nil || len(daily.Days) != 1 || daily.Days[0].Used != 15 {
		t.Errorf("current sibling's credits were not recorded: %+v", daily)
	}
}

func changeWhileHistoryWaits(t *testing.T, mutex sync.Locker, note func(context.Context), change func(*testing.T)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		done := make(chan struct{})
		mutex.Lock()
		go func() {
			defer close(done)
			note(t.Context())
		}()
		defer func() {
			mutex.Unlock()
			<-done
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("history completed before the held writer lock was released")
		default:
		}
		change(t)
	})
}
