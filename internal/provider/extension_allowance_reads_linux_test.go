package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionAllowanceBatchesUsageRevisions(t *testing.T) {
	fixture := keepAliveUsageFixture(t, "synthetic-allowed")
	startWithNoAllowancesRead(t)
	store := fixture.manager.Store()
	id := ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider)
	user := fixture.scope.Account
	accountPath := filepath.Join(fixture.manager.Namespace(), "private", fixture.scope.Plugin, "accounts.json")
	cache := &usedCache
	cache.Lock()
	priorBindings, priorReads := cache.extensions, cache.read
	cache.extensions, cache.read = map[string]map[string]string{}, nil
	cache.Unlock()
	t.Cleanup(func() {
		cache.Lock()
		cache.extensions, cache.read = priorBindings, priorReads
		cache.Unlock()
	})

	saved, singleAllowances, singleStatus := 1, 0, 0
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
			logins, external := extensionLoginsContext(t.Context(), id)
			if !external || len(logins) != count {
				t.Fatalf("discovered %d SDK accounts, want %d", len(logins), count)
			}
			before := make(map[string]Allowance, count)
			bindings := make(map[string]string, count)
			readings := make(map[string]readState, count)
			now := time.Now()
			var selected sdk.Scope
			for _, login := range logins {
				before[login.User] = Allowance{{Used: 40}}
				bindings[login.User] = loginReadingCacheKeyContext(t.Context(), login)
				readings[id+"/"+strings.ToLower(login.User)] = readState{at: now}
				if login.User == user {
					selected = login.extension.currentScope()
				}
			}
			if selected.Account == "" || len(before) != count {
				t.Fatal("missing or duplicate fixture identities")
			}
			cache.Lock()
			cache.m[id], cache.at[id], cache.extensions[id], cache.read = before, now, bindings, readings
			cache.Unlock()

			assertCached := func(subject string) {
				t.Helper()
				if at, failed, ok := UsageRead(id, subject); !ok || failed || !at.Equal(now) {
					t.Errorf("warm UsageRead(%s) = %v, failed %t, present %t", subject, at, failed, ok)
				}
			}
			var returned map[string]Allowance
			checkAllowances := func() {
				t.Helper()
				returned = Allowances(id)
				if len(returned) != count || len(returned[user]) != 1 || returned[user][0].Used != 40 {
					t.Fatalf("warm Allowances = %+v, want %d accounts with cached usage", returned, count)
				}
			}

			checkAllowances()
			assertCached(user)
			allowances := countProviderCatalogReads(t, accountPath, checkAllowances)
			status := countProviderCatalogReads(t, accountPath, func() { assertCached(user) })
			t.Logf("%d SDK accounts: warm Allowances %d accounts.json reads, UsageRead %d", count, allowances, status)
			if count == 1 {
				singleAllowances, singleStatus = allowances, status
			}
			if singleAllowances == 0 || singleStatus == 0 || allowances != singleAllowances || status != singleStatus {
				t.Errorf("reads = %d/%d, want the one-account count %d/%d", allowances, status, singleAllowances, singleStatus)
			}
			if count == 100 {
				scope, err := json.Marshal(selected)
				if err != nil {
					t.Fatal(err)
				}
				peer := proc.CommandContext(t.Context(), os.Args[0], "-test.run=^TestExtensionUsageChild$")
				peer.Env = append(os.Environ(), "SDK_USAGE_CHILD_ROOT="+fixture.manager.Namespace(), "SDK_USAGE_CHILD_SCOPE="+string(scope))
				if output, err := peer.CombinedOutput(); err != nil {
					t.Fatalf("foreign usage invalidation: %v\n%s", err, output)
				}
				if at, _, ok := UsageRead(id, user); ok || !at.IsZero() {
					t.Errorf("UsageRead kept invalidated account %s at %v", user, at)
				}
				assertCached("account-1")
				assertCached("account-99")
				cache.Lock()
				remaining := cache.m[id]
				running := cache.loading[id] != nil
				statuses := len(cache.read)
				cache.Unlock()
				if len(remaining) != count-1 || statuses != count-1 || remaining[user] != nil ||
					len(returned) != count || len(returned[user]) != 1 || running {
					t.Fatal("invalidation failed to isolate the changed account, mutated the warm snapshot, or started a refresh")
				}
			}
			if fixture.manager.Running(fixture.scope.Plugin) || fixture.calls.Load() != 0 {
				t.Fatal("warm routing launched a worker or vendor call")
			}
		})
	}
}
