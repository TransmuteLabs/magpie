package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/agentenv"
	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type storedQuotaHistoryHost struct {
	*fixtureExtensionProviderHost
	afterRead func(context.Context)
}

func (host storedQuotaHistoryHost) ModuleConfig(ctx context.Context, id string) (sdk.ConfigSnapshot, error) {
	host.afterRead(ctx)
	return host.fixtureExtensionProviderHost.ModuleConfig(ctx, id)
}

func TestLoginUsageHistoryHonorsCallerDeadline(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	id := ExtensionProviderID(host.manifest.ID, "vendor")
	lock, err := os.OpenFile(filepath.Join(host.Namespace(), "private", ".lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	var vendorAnswered atomic.Bool
	host.call = func(_ context.Context, method string, scope sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if method != sdk.MethodProviderUsage || scope.Account != "alice" {
			t.Errorf("unexpected quota call %s for %q", method, scope.Account)
		}
		vendorAnswered.Store(true)
		return json.RawMessage(`{"windows":[{"name":"quota","used":40}]}`), nil
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		deadline, _ := ctx.Deadline()
		held := make(chan bool, 1)
		hold := sync.OnceFunc(func() {
			ok, err := filelock.TryLock(lock)
			held <- ok && err == nil
		})
		release := sync.OnceFunc(func() { filelock.Unlock(lock) })
		defer release()
		providerExtensionHost = func() extensionProviderHost {
			return storedQuotaHistoryHost{host, func(ctx context.Context) {
				limit, _ := ctx.Deadline()
				if vendorAnswered.Load() && limit.Equal(deadline) {
					hold()
				}
			}}
		}
		done := make(chan map[string]SubscriptionQuota, 1)
		go func() { done <- someLoginUsage(ctx, id, "alice") }()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case locked := <-held:
			if !locked {
				t.Error("could not lock the Store after validating the quota")
			}
		default:
			t.Error("quota did not reach the final caller-side proof")
		}
		select {
		case readings := <-done:
			if result := readings["alice"]; len(result.Windows) != 1 || result.Windows[0].Used != 40 {
				t.Errorf("deadline lost a completed reading: %+v", result)
			}
		default:
			release()
			<-done
			t.Error("account quota history waited for the Store after the caller expired")
		}
	})
	if _, err := os.Stat(quotaHistPath()); !os.IsNotExist(err) {
		t.Errorf("expired read published quota history: %v", err)
	}
	providerExtensionHost = func() extensionProviderHost { return host }
	reading := someLoginUsage(t.Context(), id, "alice")["alice"]
	if len(reading.Windows) != 1 || reading.Windows[0].Used != 40 {
		t.Fatalf("account quota did not recover: %+v", reading)
	}
	if _, err := os.Stat(quotaHistPath()); err != nil {
		t.Fatal("fresh read did not publish quota history:", err)
	}
}

func TestRefreshReportHistoryHonorsCallerDeadline(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	id := ExtensionProviderID(host.manifest.ID, "vendor")
	if err := Save(Provider{ID: id, Proxy: "direct"}); err != nil {
		t.Fatal(err)
	}
	previous := SubscriptionQuota{Provider: id, User: "alice", Windows: []QuotaWindow{{Name: "quota", Used: 25}}}
	warmSubscriptionUsageForTest(t, previous)
	lock := filelock.Mutex{Path: func() string { return filepath.Join(host.Namespace(), "private", ".lock") }}
	var vendorAnswered atomic.Bool
	host.call = func(_ context.Context, method string, scope sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if method != sdk.MethodProviderUsage || scope.Account != "alice" {
			t.Errorf("unexpected quota call %s for %q", method, scope.Account)
		}
		vendorAnswered.Store(true)
		return json.RawMessage(`{"windows":[{"name":"quota","used":40}]}`), nil
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		deadline, _ := ctx.Deadline()
		held := make(chan func(), 1)
		hold := sync.OnceFunc(func() {
			if lock.LockCheckedContext(ctx) {
				held <- lock.Unlock
			} else {
				held <- nil
			}
		})
		providerExtensionHost = func() extensionProviderHost {
			return storedQuotaHistoryHost{host, func(ctx context.Context) {
				limit, _ := ctx.Deadline()
				if vendorAnswered.Load() && limit.Equal(deadline) && netproxy.Choice(ctx) == "direct" {
					hold()
				}
			}}
		}
		done := make(chan []Quota, 1)
		go func() { done <- RefreshReport(ctx, id, "alice", time.Now()) }()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		release := func() {}
		select {
		case unlock := <-held:
			if unlock == nil {
				t.Error("fixture could not hold the Store after the quota read")
			} else {
				release = sync.OnceFunc(unlock)
			}
		default:
			t.Error("refresh did not reach the post-vendor proof")
		}
		defer release()
		select {
		case cards := <-done:
			if len(cards) != 0 {
				t.Errorf("expired refresh returned %d cards", len(cards))
			}
		default:
			release()
			<-done
			t.Error("quota history or report naming outlasted the caller deadline")
		}
	})
	cache := &subscriptionUsageCache
	cache.Lock()
	cards := cache.data
	cache.Unlock()
	if len(cards) != 1 || len(cards[0].Windows) != 1 || cards[0].Windows[0].Used != 25 {
		t.Errorf("expired refresh replaced the previous card: %+v", cards)
	}
	if _, err := os.Stat(quotaHistPath()); !os.IsNotExist(err) {
		t.Errorf("expired refresh recorded quota history: %v", err)
	}
	providerExtensionHost = func() extensionProviderHost { return host }
	if cards := RefreshReport(t.Context(), id, "alice", time.Now()); len(cards) != 1 ||
		len(cards[0].Windows) != 1 || cards[0].Windows[0].Used != 40 {
		t.Fatalf("a later refresh failed to recover: %+v", cards)
	}
}

// A card's refresh button reads that card alone (Hu9956, #840): the key
// it is for is asked again and the cache has its new figure, while the
// provider's other key and another provider's card aren't asked and keep
// what they read.
func TestRefreshOneCard(t *testing.T) {
	isolate(t)
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h, ".cache"))
	t.Setenv("PATH", h)
	for _, v := range agentenv.Vars {
		t.Setenv(v, "")
	}
	keyBalanceCache.data = nil
	t.Cleanup(func() { keyBalanceCache.data = nil })

	var mu sync.Mutex
	asked := map[string]int{}
	left := map[string]string{"Bearer sk-one": "1000000", "Bearer sk-two": "250000", "Bearer sk-other": "500000"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		k := r.Header.Get("Authorization")
		asked[k]++
		w.Write([]byte(`{"data":{"total_available":` + left[k] + `}}`))
	}))
	defer srv.Close()
	for _, p := range []Provider{
		{ID: "relay", Name: "Relay", Chat: srv.URL + "/v1", Key: "sk-one", KeyName: "main", Keys: []KeyAccount{{Name: "spare", Key: "sk-two"}}},
		{ID: "other", Name: "Other", Chat: srv.URL + "/v1", Key: "sk-other"},
	} {
		p.BalanceURL, p.BalancePath = srv.URL+"/api/usage/token", "$data.total_available / 500000"
		if err := Save(p); err != nil {
			t.Fatal(err)
		}
	}
	cards := func() string {
		var lines []string
		for _, q := range KeyBalances(context.Background()) {
			lines = append(lines, q.Provider+"|"+q.User+"|"+q.Balance)
		}
		return strings.Join(lines, ",")
	}
	if got, want := cards(), "relay|main|$2.00,relay|spare|$0.50,other||$1.00"; got != want {
		t.Fatalf("balances %s, want %s", got, want)
	}
	mu.Lock()
	for k := range left {
		left[k] = "100000"
	}
	clear(asked)
	mu.Unlock()

	// the menu bar, showing the same cards, hears of the read at once,
	// not at its next tick minutes later (#2357)
	var told atomic.Int32
	OnSubscriptionUsage(func() { told.Add(1) })
	t.Cleanup(func() { OnSubscriptionUsage(nil) })
	RefreshUsage(context.Background(), "relay", "Spare")
	if got, want := cards(), "relay|main|$2.00,relay|spare|$0.20,other||$1.00"; got != want {
		t.Fatalf("after spare's refresh %s, want %s", got, want)
	}
	if told.Load() == 0 {
		t.Fatal("a card read again on its own didn't tell the menu bar")
	}
	if len(asked) != 1 || asked["Bearer sk-two"] != 1 {
		t.Fatalf("asked %v, want sk-two alone", asked)
	}
	// a card of one account: every key of the provider's is its
	clear(asked)
	RefreshUsage(context.Background(), "other", "")
	if got, want := cards(), "relay|main|$2.00,relay|spare|$0.20,other||$0.20"; got != want {
		t.Fatalf("after other's refresh %s, want %s", got, want)
	}
	if len(asked) != 1 || asked["Bearer sk-other"] != 1 {
		t.Fatalf("asked %v, want sk-other alone", asked)
	}
}
