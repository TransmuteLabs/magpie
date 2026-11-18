package provider

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionUsageChild(t *testing.T) {
	root := os.Getenv("SDK_USAGE_CHILD_ROOT")
	if root == "" {
		return
	}
	var scope sdk.Scope
	if err := json.Unmarshal([]byte(os.Getenv("SDK_USAGE_CHILD_SCOPE")), &scope); err != nil {
		t.Fatal(err)
	}
	if _, err := extensionhost.NewStore(root).InvalidateUsage(scope); err != nil {
		t.Fatal(err)
	}
}

func TestExtensionUsageForeignProcessInvalidatesWarmCache(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	host.manifest.Contract.Version.Minor = 4
	logins, _ := extensionLogins(extensionAccounts()[0].ID)
	login := logins[0]
	host.call = func(_ context.Context, _ string, _ sdk.Scope, params any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		request := params.(sdk.UsageRequest)
		return json.Marshal(sdk.UsageResult{Revision: request.Revision,
			Windows: []sdk.UsageWindow{{Name: "quota", Used: 40}}})
	}
	known := cachedLoginReading(context.Background(), login)
	if known.q.Error != "" || len(known.q.Windows) != 1 {
		t.Fatal("initial usage did not warm cache:", known.q.Error)
	}
	scope := login.extension.currentScope()
	raw, _ := json.Marshal(scope)
	command := exec.Command(os.Args[0], "-test.run=^TestExtensionUsageChild$")
	command.Env = append(os.Environ(), "SDK_USAGE_CHILD_ROOT="+host.Namespace(), "SDK_USAGE_CHILD_SCOPE="+string(raw))
	if body, err := command.CombinedOutput(); err != nil {
		t.Fatalf("foreign invalidation = %v %q", err, body)
	}
	if _, known := routedAllowance(login.Agent, login.User, known.q, time.Now()); known {
		t.Fatal("foreign-process mutation reused warm known allowance")
	}
	if ready := host.store.UsageReady(scope); ready == nil {
		t.Fatal("dirty account admitted inference through another process")
	}
	fresh := cachedLoginReading(context.Background(), login)
	if fresh.q.Error != "" || host.store.UsageReady(scope) != nil {
		t.Fatal("matched new read did not reopen admission:", fresh.q.Error)
	}
}

func TestExtensionUsageBarrierRejectsLateReadAndItsWaiter(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	host.manifest.Contract.Version.Minor = 4
	logins, _ := extensionLogins(extensionAccounts()[0].ID)
	login := logins[0]
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	host.call = func(_ context.Context, method string, scope sdk.Scope, params any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		request := params.(sdk.UsageRequest)
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return json.Marshal(sdk.UsageResult{Revision: request.Revision,
			Windows: []sdk.UsageWindow{{Name: "five hours", Used: 12}}})
	}
	result := make(chan loginUsageEntry, 1)
	go func() { result <- cachedLoginReading(context.Background(), login) }()
	<-started
	scope := login.extension.currentScope()
	before, _ := host.store.GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if _, err := host.store.InvalidateUsage(scope); err != nil {
		t.Fatal(err)
	}
	close(release)
	late := <-result
	if late.q.Error == "" || len(late.q.Windows) != 0 || !late.q.ReadDeferred {
		t.Fatal("pre-mutation read escaped to its existing waiter")
	}
	if err := host.store.UsageReady(scope); err == nil {
		t.Fatal("late read reopened inference")
	}
	fresh := cachedLoginReading(context.Background(), login)
	if fresh.q.Error != "" || len(fresh.q.Windows) != 1 {
		t.Fatalf("matched revision failed: %s", fresh.q.Error)
	}
	if err := host.store.UsageReady(scope); err != nil {
		t.Fatal(err)
	}
	after, _ := host.store.GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if before.Generation != after.Generation || string(before.Auth) != string(after.Auth) {
		t.Fatal("usage invalidation rotated credentials")
	}
}

func TestExtensionUsageApplicabilityRoutesExistingQuotaWindows(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	host.manifest.Contract.Version.Minor = 4
	provider := extensionAccounts()[0]
	logins, _ := extensionLogins(provider.ID)
	host.call = func(_ context.Context, _ string, _ sdk.Scope, params any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		request := params.(sdk.UsageRequest)
		return json.Marshal(sdk.UsageResult{Revision: request.Revision, Windows: []sdk.UsageWindow{
			{Name: "ordinary", Used: 100, NotModels: []string{"model-OffPeak", "model-Highspeed"}},
			{Name: "mcp", Used: 100, Aside: true},
		}})
	}
	quota := extensionQuota(context.Background(), logins[0])
	if quota.Error != "" {
		t.Fatal(quota.Error)
	}
	allowance, known := routedAllowance(provider.ID, logins[0].User, quota, time.Now())
	ordinary, _ := allowance.For("model", time.Now())
	alias, _ := allowance.For("MODEL-offpeak", time.Now())
	if !known || ordinary != 100 || alias != 0 {
		t.Fatalf("selector routing: known=%v ordinary=%v alias=%v", known, ordinary, alias)
	}
	if _, err := host.store.InvalidateUsage(logins[0].extension.currentScope()); err != nil {
		t.Fatal(err)
	}
	if _, known := routedAllowance(provider.ID, logins[0].User, quota, time.Now()); known {
		t.Fatal("invalidated known allowance remained authoritative")
	}
}

func TestExtensionUsageEpochDoesNotChangeModelIdentityOrOtherAccount(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	all := extensionAccounts()
	first, second := all[0].Account.extension, all[0].AlsoOn()[0].Account.extension
	modelKey, usageKey := first.cacheKey(), extensionUsageKey(first)
	otherKey := extensionUsageKey(second)
	if _, err := host.store.InvalidateUsage(first.currentScope()); err != nil {
		t.Fatal(err)
	}
	if first.cacheKey() != modelKey || extensionUsageKey(first) == usageKey || extensionUsageKey(second) != otherKey {
		t.Fatal("invalidation touched model identity or another account")
	}
	if !strings.Contains(extensionUsageKey(first), "usage:1") {
		t.Fatal("usage revision missing from cache identity")
	}
}

func TestExtensionUsageDiskSnapshotCannotRestoreAuthority(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	host.manifest.Contract.Version.Minor = 4
	provider := extensionAccounts()[0]
	logins, _ := extensionLogins(provider.ID)
	host.call = func(_ context.Context, _ string, _ sdk.Scope, params any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		request := params.(sdk.UsageRequest)
		return json.Marshal(sdk.UsageResult{Revision: request.Revision, Windows: []sdk.UsageWindow{{Name: "quota", Used: 40}}})
	}
	quota := extensionQuota(context.Background(), logins[0])
	if _, known := routedAllowance(provider.ID, logins[0].User, quota, time.Now()); !known {
		t.Fatal("fresh scoped quota was not known")
	}
	raw, err := json.Marshal(quota)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded SubscriptionQuota
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatal(err)
	}
	if _, err := host.store.InvalidateUsage(logins[0].extension.currentScope()); err != nil {
		t.Fatal(err)
	}
	if _, known := routedAllowance(provider.ID, logins[0].User, reloaded, time.Now()); known {
		t.Fatal("disk snapshot restored known quota without a matched live acknowledgment")
	}
}

func TestExtensionUsageAsideUnknownDoesNotHideOrdinaryQuota(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	host.manifest.Contract.Version.Minor = 4
	provider := extensionAccounts()[0]
	logins, _ := extensionLogins(provider.ID)
	ordinaryUnknown := false
	host.call = func(_ context.Context, _ string, _ sdk.Scope, params any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		request := params.(sdk.UsageRequest)
		return json.Marshal(sdk.UsageResult{Revision: request.Revision, Windows: []sdk.UsageWindow{
			{Name: "ordinary", Used: 40, Unknown: ordinaryUnknown},
			{Name: "mcp", Unknown: true, Aside: true},
		}})
	}
	quota := extensionQuota(context.Background(), logins[0])
	allowance, known := routedAllowance(provider.ID, logins[0].User, quota, time.Now())
	used, _ := allowance.For("model", time.Now())
	if !known || used != 40 {
		t.Fatalf("informational unknown hid ordinary quota: known=%v used=%v", known, used)
	}
	ordinaryUnknown = true
	quota = extensionQuota(context.Background(), logins[0])
	if _, known := routedAllowance(provider.ID, logins[0].User, quota, time.Now()); known {
		t.Fatal("unknown ordinary quota became authoritative")
	}
}

func TestExtensionUsageBindingInvalidationPreservesReturnedSnapshot(t *testing.T) {
	agent := "extension-usage-binding-test"
	cache := &usedCache
	cache.Lock()
	defer cache.Unlock()
	if cache.m == nil {
		cache.m = map[string]map[string]Allowance{}
	}
	if cache.extensions == nil {
		cache.extensions = map[string]map[string]string{}
	}
	defer delete(cache.m, agent)
	defer delete(cache.extensions, agent)
	defer delete(cache.at, agent)
	delete(cache.m, agent)
	cache.extensions[agent] = map[string]string{"alice": "revision0", "bob": "revision0"}
	syncExtensionAllowanceBindings(agent, map[string]string{"alice": "revision1", "bob": "revision0"})
	if _, exists := cache.m[agent]; exists {
		t.Fatal("invalidation invented a warm cache before its first reading")
	}
	snapshot := map[string]Allowance{"alice": {{Used: 40}}, "bob": {{Used: 60}}}
	cache.m[agent] = snapshot
	cache.extensions[agent] = map[string]string{"alice": "revision0", "bob": "revision0"}
	syncExtensionAllowanceBindings(agent, map[string]string{"alice": "revision1", "bob": "revision0"})
	if len(snapshot) != 2 || len(snapshot["alice"]) != 1 {
		t.Fatal("invalidation mutated an allowance map already returned to another caller")
	}
	if _, stale := cache.m[agent]["alice"]; stale || len(cache.m[agent]["bob"]) != 1 {
		t.Fatal("invalidation failed to isolate the changed account")
	}
}
