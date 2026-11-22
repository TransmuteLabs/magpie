package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestDailyCreditsRefreshOrdersRepliesAcrossProcesses(t *testing.T) {
	const peerEnv = "MAGPIE_TEST_CREDITS_REFRESH_PEER"
	const user, window = "alice", "Credits"
	if config := os.Getenv(peerEnv); config != "" {
		refresh := dailyCreditsRefresh(t, config, func(context.Context) (float64, error) {
			return 20, nil
		})
		assertDailyCreditsRefresh(t, refresh(t.Context()), user, 20)
		return
	}

	for _, initial := range []float64{10, 20} {
		t.Run(fmt.Sprintf("last=%g", initial), func(t *testing.T) {
			isolateRefreshes(t)
			config := t.TempDir()
			arrived, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			var calls atomic.Int32
			refresh := dailyCreditsRefresh(t, config, func(ctx context.Context) (float64, error) {
				switch calls.Add(1) {
				case 1:
					close(arrived)
					select {
					case <-release:
						return 15, nil
					case <-ctx.Done():
						return 0, ctx.Err()
					}
				case 2:
					return 20, nil
				case 3:
					return 8, nil
				case 4:
					return 11, nil
				default:
					return 0, fmt.Errorf("unexpected vendor read")
				}
			})
			id := ExtensionProviderID("fixture.external", "vendor")
			key := creditsKey(SubscriptionQuota{Provider: id, User: user}, window)
			yesterday := time.Now().AddDate(0, 0, -1)
			day := yesterday.Local().Format("2006-01-02")
			writeFile(t, creditsPath(), map[string]*creditsAccount{
				key: {Since: day, Last: creditsReading{Used: initial, Total: 100, At: yesterday},
					Days: map[string]float64{day: 7}},
			})
			checkHistory := func(last, spent float64) {
				t.Helper()
				b, err := os.ReadFile(creditsPath())
				if err != nil {
					t.Fatal(err)
				}
				history, err := parseCredits(b)
				if err != nil {
					t.Fatal(err)
				}
				record := history[key]
				if record == nil || record.Since != day || record.Days[day] != 7 {
					t.Fatalf("previous history was lost: %+v", record)
				}
				total := -record.Days[day]
				for _, amount := range record.Days {
					total += amount
				}
				if record.Last.Used != last || total != spent {
					t.Errorf("daily credits after refresh: last=%g spent=%g, want last=%g spent=%g",
						record.Last.Used, total, last, spent)
				}
			}

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var late []Quota
			done := make(chan struct{})
			go func() {
				defer close(done)
				late = refresh(ctx)
			}()
			defer func() {
				unblock()
				<-done
			}()
			select {
			case <-arrived:
			case <-ctx.Done():
				t.Fatal("the older refresh never reached the provider")
			}
			child := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestDailyCreditsRefreshOrdersRepliesAcrossProcesses$", "-test.count=1")
			child.Env = append(os.Environ(), peerEnv+"="+config, "GORACE=atexit_sleep_ms=0")
			if output, err := child.CombinedOutput(); err != nil {
				t.Fatalf("the second process could not refresh while the first was waiting: %v\n%s", err, output)
			}
			spent := 20 - initial
			checkHistory(20, spent)
			unblock()
			<-done
			assertDailyCreditsRefresh(t, late, user, 15)
			checkHistory(20, spent)
			assertDailyCreditsRefresh(t, refresh(ctx), user, 20)
			checkHistory(20, spent)
			assertDailyCreditsRefresh(t, refresh(ctx), user, 8)
			checkHistory(8, spent)
			assertDailyCreditsRefresh(t, refresh(ctx), user, 11)
			checkHistory(11, spent+3)
			before, err := os.Stat(creditsPath())
			if err != nil {
				t.Fatal(err)
			}
			cached := cardOf(SubscriptionUsage(ctx), id, user)
			endReadsOut(t, "after displaying the cached credit reading")
			if cached == nil || cached.Daily == nil || len(cached.Windows) != 1 || cached.Windows[0].Amount != 11 {
				t.Fatalf("the Usage page did not retain the cached reading and its daily totals: %+v", cached)
			}
			if after, err := os.Stat(creditsPath()); err != nil || !os.SameFile(before, after) {
				t.Errorf("showing the same cached reading rewrote the credit history: %v", err)
			}
			if calls.Load() != 4 {
				t.Errorf("the first process made %d vendor reads, want 4", calls.Load())
			}
		})
	}
}

func dailyCreditsRefresh(t *testing.T, config string, read func(context.Context) (float64, error)) func(context.Context) []Quota {
	t.Helper()
	host := fixtureExtensionProvider(t, false)
	host.namespace = filepath.Join(config, "extensions")
	host.store = extensionhost.NewStore(host.namespace)
	t.Setenv("XDG_CONFIG_HOME", config)
	host.manifest.Contract.Version.Minor = 9
	host.manifest.Contract.Capabilities = append(host.manifest.Contract.Capabilities, sdk.AccountAuth)
	if _, err := host.Store().GetAccount(host.manifest.ID, "vendor", "alice"); err != nil {
		if _, err := host.Store().SaveAccount(extensionhost.Account{Plugin: host.manifest.ID, Provider: "vendor", ID: "alice",
			Auth: json.RawMessage(`{"access":"synthetic-credit-reader"}`)}, 0); err != nil {
			t.Fatal(err)
		}
	}
	host.call = func(ctx context.Context, method string, scope sdk.Scope, params any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if method != sdk.MethodProviderUsage || scope.Account != "alice" {
			return nil, fmt.Errorf("unexpected request %s for %s", method, scope.Account)
		}
		used, err := read(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(sdk.UsageResult{Revision: params.(sdk.UsageRequest).Revision,
			Windows: []sdk.UsageWindow{{Name: "Credits", Used: used, Unit: "credits", Amount: &used, Limit: 100}}})
	}
	id := ExtensionProviderID(host.manifest.ID, "vendor")
	return func(ctx context.Context) []Quota { return RefreshReport(ctx, id, "alice", time.Now()) }
}

func assertDailyCreditsRefresh(t *testing.T, got []Quota, user string, used float64) {
	t.Helper()
	if len(got) != 1 || got[0].User != user || got[0].Error != "" || got[0].AsOf != nil ||
		len(got[0].Windows) != 1 || got[0].Windows[0].Amount != used {
		t.Fatalf("refresh should show %g credits used: %+v", used, got)
	}
}
