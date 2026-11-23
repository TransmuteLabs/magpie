package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionCardRefreshUpdatesRoutingAllowance(t *testing.T) {
	for _, scenario := range []struct {
		name, batch                  string
		unknownAside, unknownReserve bool
		invalidate                   bool
	}{
		{name: "warm"},
		{name: "older-batch-in-flight", batch: "older"},
		{name: "account-absent-from-batch", batch: "missing"},
		{name: "old-binding-invalidated", batch: "missing", invalidate: true},
		{name: "unknown-informational-window", unknownAside: true},
		{name: "unknown-ordinary-window", unknownReserve: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			host := fixtureExtensionProvider(t, true)
			host.manifest.Contract.Version.Minor = 4
			agent := ExtensionProviderID(host.manifest.ID, "vendor")
			var aliceUsed, aliceCalls, bobCalls atomic.Int32
			aliceUsed.Store(100)
			host.call = func(_ context.Context, method string, scope sdk.Scope, params any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
				if method != sdk.MethodProviderUsage || scope.Provider != "vendor" {
					return nil, fmt.Errorf("unexpected fixture call %s for %v", method, scope)
				}
				var used float64
				switch scope.Account {
				case "alice":
					aliceCalls.Add(1)
					used = float64(aliceUsed.Load())
				case "bob":
					bobCalls.Add(1)
					used = 30
				default:
					return nil, fmt.Errorf("unexpected fixture scope %v", scope)
				}
				request := params.(sdk.UsageRequest)
				windows := []sdk.UsageWindow{
					{Name: "Paid", Used: 20, Models: []string{"paid"}},
					{Name: "Trial reserve", Used: used, Models: []string{"trial"}, Unknown: used == 60 && scenario.unknownReserve},
				}
				if scope.Account == "alice" && used == 60 && scenario.unknownAside {
					windows = append(windows, sdk.UsageWindow{Name: "MCP", Aside: true, Unknown: true})
				}
				return json.Marshal(sdk.UsageResult{Revision: request.Revision, Windows: windows})
			}

			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			if !ReadUsageNow(ctx, agent, "alice", "bob") {
				t.Fatal("could not warm routing and the login readings")
			}
			endReadsOut(t, "after warming routing")
			held := Allowances(agent)
			aliceAt, aliceFailed, aliceKnown := UsageRead(agent, "alice")
			bobAt, failed, ok := UsageRead(agent, "bob")
			now := time.Now()
			if held["alice"].CapUsed("trial", now) != 100 || held["alice"].CapUsed("paid", now) != 20 ||
				held["bob"].CapUsed("trial", now) != 30 || !ok || failed || !aliceKnown || aliceFailed ||
				aliceCalls.Load() != 1 || bobCalls.Load() != 1 {
				t.Fatalf("initial route or reading counts are wrong: %v, calls %d/%d", held, aliceCalls.Load(), bobCalls.Load())
			}

			expireRouting := func() {
				usedCache.Lock()
				usedCache.at[agent] = time.Time{}
				usedCache.Unlock()
				Allowances(agent)
			}
			var finishBatch func()
			switch scenario.batch {
			case "older":
				captured, proceed := make(chan []SubscriptionQuota, 1), make(chan struct{})
				before := noteHistoryWrite
				noteHistoryWrite = func(ctx context.Context, quotas []SubscriptionQuota, now time.Time) {
					captured <- quotas
					<-proceed
					before(ctx, quotas, now)
				}
				finishBatch = sync.OnceFunc(func() {
					close(proceed)
					endReadsOut(t, "after releasing the held batch")
					noteHistoryWrite = before
				})
				defer finishBatch()
				expireRouting()
				select {
				case quotas := <-captured:
					if !slices.ContainsFunc(quotas, func(q SubscriptionQuota) bool {
						return q.User == "alice" && len(q.Windows) == 2 && q.Windows[1].Used == 100
					}) {
						t.Fatalf("held routing batch did not capture Alice's old reserve: %+v", quotas)
					}
				case <-ctx.Done():
					t.Fatal("routing did not capture its old account readings")
				}
				if aliceCalls.Load() != 1 || bobCalls.Load() != 1 {
					t.Fatal("the routing-only cache expiration also expired a login reading")
				}
			case "missing":
				bob, ok := knownLoginUsage(agent)["bob"]
				if !ok {
					t.Fatal("the batch fixture has no cached reading for Bob")
				}
				captured, proceed := make(chan string, 1), make(chan struct{})
				LoginUsageVia(func(ctx context.Context, selected string) map[string]SubscriptionQuota {
					select {
					case captured <- selected:
					default:
					}
					select {
					case <-proceed:
						return map[string]SubscriptionQuota{"bob": bob}
					case <-ctx.Done():
						return nil
					}
				})
				finishBatch = sync.OnceFunc(func() {
					close(proceed)
					endReadsOut(t, "after releasing the batch without Alice")
					LoginUsageVia(nil)
				})
				defer finishBatch()
				expireRouting()
				select {
				case selected := <-captured:
					if selected != agent {
						t.Fatalf("captured the wrong provider's batch: %q", selected)
					}
				case <-ctx.Done():
					t.Fatal("routing did not reach the batch without Alice")
				}
			}

			aliceUsed.Store(60)
			cards := RefreshReport(ctx, agent, "alice", now)
			windowCount := 2
			if scenario.unknownAside {
				windowCount++
			}
			if len(cards) != 1 || cards[0].User != "alice" || len(cards[0].Windows) != windowCount ||
				cards[0].Error != "" || cards[0].ReadDeferred || !slices.ContainsFunc(cards[0].Windows, func(window QuotaSpan) bool {
				return window.Name == "Trial reserve" && window.Unknown == scenario.unknownReserve &&
					(scenario.unknownReserve || window.Used == 60)
			}) {
				t.Fatalf("Usage refresh did not publish Alice's fresh reserve: %+v", cards)
			}
			if scenario.unknownReserve {
				if got := Allowances(agent)["alice"].CapUsed("trial", now); got != 100 {
					t.Errorf("unknown ordinary reading changed routing to %g%%, want the previous 100%%", got)
				}
				if at, failed, ok := UsageRead(agent, "alice"); !ok || failed || !at.Equal(aliceAt) {
					t.Errorf("unknown ordinary reading changed the prior routed status: %v, %v, %v", at, failed, ok)
				}
				expireRouting()
				endReadsOut(t, "after rebuilding routing with unknown ordinary usage")
				if _, known := Allowances(agent)["alice"]; known {
					t.Error("the routing batch accepted a reading with an unknown ordinary window")
				}
				if at, failed, ok := UsageRead(agent, "alice"); !ok || !failed || !at.Equal(aliceAt) {
					t.Errorf("ordinary unknown batch read status: %v, %v, %v", at, failed, ok)
				}
				if aliceCalls.Load() != 2 || bobCalls.Load() != 1 {
					t.Errorf("ordinary unknown case made %d/%d Alice/Bob calls, want 2/1", aliceCalls.Load(), bobCalls.Load())
				}
				return
			}
			if scenario.unknownAside && !slices.ContainsFunc(cards[0].Windows, func(window QuotaSpan) bool {
				return window.Name == "MCP" && window.Unknown && window.Text
			}) {
				t.Fatalf("Usage refresh did not keep the unknown informational window: %+v", cards)
			}
			if got := Allowances(agent)["alice"].CapUsed("trial", now); got != 60 {
				t.Errorf("fresh Usage left the routing allowance at %g%%, want 60%%", got)
			}
			if at, failed, ok := UsageRead(agent, "alice"); scenario.unknownAside {
				if !ok || !failed || !at.Equal(aliceAt) {
					t.Error("unknown informational window did not preserve the last good read time with failed status")
				}
			} else if !ok || failed {
				t.Errorf("fresh Usage routing read status: %v, %v, %v", at, failed, ok)
			}
			if at, failed, ok := UsageRead(agent, "bob"); !ok || failed || !at.Equal(bobAt) {
				t.Errorf("refresh changed the sibling account's read status: %v, %v, %v", at, failed, ok)
			}
			if scenario.invalidate {
				scope := sdk.Scope{Plugin: host.manifest.ID, Provider: "vendor", Account: "alice", Generation: 1}
				if _, err := host.store.InvalidateUsage(scope); err != nil {
					t.Fatal(err)
				}
				finishBatch()
				if _, _, ok := UsageRead(agent, "alice"); ok {
					t.Error("the old binding's refreshed status survived invalidation")
				}
				usedCache.Lock()
				routed := usedCache.m[agent]
				_, retained := routed["alice"]
				usedCache.Unlock()
				if retained || routed["bob"].CapUsed("trial", now) != 30 {
					t.Error("the invalidated batch retained Alice or lost Bob")
				}
				if aliceCalls.Load() != 2 || bobCalls.Load() != 1 {
					t.Errorf("invalidating A made %d/%d Alice/Bob calls, want 2/1", aliceCalls.Load(), bobCalls.Load())
				}
				return
			}
			if finishBatch != nil {
				finishBatch()
			}
			routed := Allowances(agent)
			if got := routed["alice"].CapUsed("trial", now); got != 60 {
				t.Errorf("routing after the Usage refresh and any pending batch is %g%%, want 60%%", got)
			}
			if got := routed["alice"].CapUsed("paid", now); got != 20 {
				t.Errorf("routing after the Usage refresh and any pending batch lost Alice's paid window: got %g%%, want 20%%", got)
			}
			if routed["bob"].CapUsed("trial", now) != 30 || held["alice"].CapUsed("trial", now) != 100 {
				t.Error("refresh changed a sibling account or a previously returned map")
			}
			if scenario.batch != "missing" {
				if at, failed, ok := UsageRead(agent, "bob"); !ok || failed || !at.Equal(bobAt) {
					t.Errorf("the ordinary batch changed the sibling account's read status: %v, %v, %v", at, failed, ok)
				}
			}
			expireRouting()
			endReadsOut(t, "after the routing-only read")
			if got := Allowances(agent)["alice"].CapUsed("trial", now); got != 60 {
				t.Errorf("routing-only rebuild from cached Usage = %g%%, want 60%%", got)
			}
			if aliceCalls.Load() != 2 || bobCalls.Load() != 1 {
				t.Errorf("refresh made %d/%d Alice/Bob calls, want 2/1 across warmup and refresh", aliceCalls.Load(), bobCalls.Load())
			}
		})
	}
}
