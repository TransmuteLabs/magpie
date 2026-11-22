package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type heldQuotaHost struct {
	*extensionhost.Manager
	wait <-chan struct{}
}

func (host heldQuotaHost) Call(ctx context.Context, id, method string, scope sdk.Scope, params any, consume func(extensionhost.Event) error) (json.RawMessage, error) {
	if method == sdk.MethodProviderUsage {
		select {
		case <-host.wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return host.Manager.Call(ctx, id, method, scope, params, consume)
}

func TestExtensionAllowanceRejectsForeignEnablement(t *testing.T) {
	fixture := keepAliveUsageFixture(t, "synthetic-allowed")
	startWithNoAllowancesRead(t)
	id := ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider)
	user := fixture.scope.Account
	known := readAllowancesAgain(t, id)
	if limits := known[user]; len(limits) != 1 || limits[0].Used != 12 {
		t.Fatalf("initial ordinary allowance = %+v", known)
	}
	if at, failed, ok := UsageRead(id, user); !ok || failed || at.IsZero() {
		t.Fatalf("initial usage status = %v %t %t", at, failed, ok)
	}
	before, config, err := fixture.manager.ActiveModuleConfig(t.Context(), fixture.scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	foreign := extensionhost.NewCatalog(fixture.manager.Namespace())
	for _, enabled := range []bool{false, true} {
		if err := foreign.SetEnabled(t.Context(), fixture.scope.Plugin, enabled); err != nil {
			t.Fatal(err)
		}
	}
	after, current, err := fixture.manager.ActiveModuleConfig(t.Context(), fixture.scope.Plugin)
	if err != nil || after.Revision != before.Revision || current.Version != config.Version || after.EnableGeneration == before.EnableGeneration {
		t.Fatal("fixture did not change just the durable enablement generation", err)
	}
	release := make(chan struct{})
	providerExtensionHost = func() extensionProviderHost { return heldQuotaHost{fixture.manager, release} }
	t.Cleanup(func() {
		close(release)
		endReadsOut(t, "after the held SDK quota")
	})
	if at, _, ok := UsageRead(id, user); ok || !at.IsZero() {
		t.Errorf("re-enabled provider retained the previous UsageRead: %v", at)
	}
	if stale := Allowances(id); len(stale) != 0 {
		t.Errorf("re-enabled provider routed by the previous allowance: %+v", stale)
	}
	if len(known[user]) != 1 {
		t.Fatal("enablement invalidation modified a map already returned to a caller")
	}
}

func prepareUsageReadFixture(t *testing.T) (*fixtureExtensionProviderHost, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	host := fixtureExtensionProvider(t, true)
	host.manifest.Contract.Version.Minor = 4
	used, calls := new(atomic.Int32), new(atomic.Int32)
	used.Store(20)
	host.call = func(_ context.Context, method string, _ sdk.Scope, params any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
		if method != sdk.MethodProviderUsage {
			t.Errorf("unexpected fixture call: %s", method)
			return nil, nil
		}
		calls.Add(1)
		return json.Marshal(sdk.UsageResult{Revision: params.(sdk.UsageRequest).Revision,
			Windows: []sdk.UsageWindow{{Name: "quota", Used: float64(used.Load())}}})
	}
	return host, used, calls
}

func usageTestRead(t *testing.T, id, user string, used float64) SubscriptionQuota {
	t.Helper()
	quota := LoginUsage(t.Context(), id)[user]
	if quota.Error != "" || len(quota.Windows) != 1 || quota.Windows[0].Used != used {
		t.Fatalf("usage for %s/%s = %+v, want %g%%", id, user, quota, used)
	}
	return quota
}

func TestExtensionWarmQuotaRejectsForeignConfig(t *testing.T) {
	fixture, used, _ := prepareUsageReadFixture(t)
	host := &configCatalogProviderHost{fixture}
	providerExtensionHost = func() extensionProviderHost { return host }
	id := ExtensionProviderID(fixture.manifest.ID, "vendor")
	before := usageTestRead(t, id, "alice", 20)
	warmSubscriptionUsageForTest(t, before)
	if _, err := extensionhost.NewStore(host.Namespace()).StateCAS(sdk.Scope{Plugin: host.manifest.ID},
		"module.config", 0, json.RawMessage(`{"changed":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := routedAllowance(id, "alice", before, time.Now()); ok {
		t.Error("a warm quota still routed after a foreign configuration change")
	}
	if after := cardOf(SubscriptionUsage(t.Context()), id, "alice"); after == nil ||
		len(after.Windows) != 0 || !after.ReadDeferred {
		t.Errorf("the ordinary Usage page kept the old configuration quota: %+v", after)
	}
	used.Store(40)
	usageTestRead(t, id, "alice", 40)
}

func TestExtensionWarmQuotaDefersWithDiscoveryOff(t *testing.T) {
	fixture, _, calls := prepareUsageReadFixture(t)
	id := ExtensionProviderID(fixture.manifest.ID, "vendor")
	before := usageTestRead(t, id, "alice", 20)
	set := settings.Load()
	set.NoAccountDiscovery = true
	if err := settings.Save(set); err != nil {
		t.Fatal(err)
	}
	priorCalls := calls.Load()
	after := ValidateSubscriptionQuotas(t.Context(), []SubscriptionQuota{before})[0]
	if !after.ReadDeferred || len(after.Windows) != 0 || after.Balance != "" || calls.Load() != priorCalls {
		t.Errorf("warm SDK usage still published with discovery off: %+v", after)
	}
	if _, ok := routedAllowance(id, "alice", before, time.Now()); ok {
		t.Error("warm quota still routes with discovery off")
	}
}

func usageTestSubscriptionOwner(t *testing.T, host *fixtureExtensionProviderHost, publicID string) {
	t.Helper()
	dir := filepath.Join(host.Namespace(), "private", ".saved-signins")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	row := struct {
		extensionhost.SubscriptionOwner
		GrantRevision string            `json:"grantRevision"`
		Digests       map[string]string `json:"digests"`
		Tickets       []string          `json:"tickets"`
	}{
		SubscriptionOwner: extensionhost.SubscriptionOwner{
			Module: host.manifest.ID, Provider: "vendor", PublicID: publicID, Revision: "usage-fixture-owner",
			Active: true, Accounts: []extensionhost.SubscriptionAccount{{Key: "key-a", ID: "alice", User: "alice", LoginID: "login-a"}},
		},
		GrantRevision: strings.Repeat("a", 64),
		Digests:       map[string]string{"key-a": strings.Repeat("b", 64)},
		Tickets:       []string{"ticket-a"},
	}
	raw, err := json.Marshal([]any{row})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "subscription-owners.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestExtensionWarmQuotaRejectsFirstOwner(t *testing.T) {
	host, used, _ := prepareUsageReadFixture(t)
	id := ExtensionProviderID(host.manifest.ID, "vendor")
	before := usageTestRead(t, id, "alice", 20)
	const publicID = "transferred-subscription"
	usageTestSubscriptionOwner(t, host, publicID)
	if owners, err := host.Store().SubscriptionOwners(t.Context()); err != nil || len(owners) != 1 || !owners[0].Active {
		t.Fatal("fixture did not establish its first active subscription owner", err)
	}
	if _, ok := routedAllowance(id, "alice", before, time.Now()); ok {
		t.Error("a warm quota captured without an owner still routed after the first cutover")
	}
	used.Store(40)
	usageTestRead(t, publicID, "alice", 40)
}
