package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestKeptQuotaSnapshotChecksEachAccountAndModule(t *testing.T) {
	fixture := keepAliveUsageFixture(t, "synthetic-allowed")
	store := fixture.manager.Store()
	add := func(module, provider, user string) {
		t.Helper()
		if _, err := store.SaveAccount(extensionhost.Account{
			Plugin: module, Provider: provider, ID: user,
			Auth: json.RawMessage(`{"token":"synthetic-allowed"}`), ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		}, 0); err != nil {
			t.Fatal(err)
		}
	}
	add(fixture.scope.Plugin, fixture.scope.Provider, "second")
	other, err := fixture.manager.Manifest(fixture.scope.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	other.ID = "other.usage-fixture"
	other.Providers[0].ID = "other-vendor"
	otherDir := t.TempDir()
	worker, err := os.ReadFile(filepath.Join(fixture.directory, other.Entry))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, other.Entry), worker, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.manager.Install(t.Context(), otherDir); err != nil {
		t.Fatal(err)
	}
	add(other.ID, "other-vendor", "third")
	firstID := ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider)
	otherID := ExtensionProviderID(other.ID, "other-vendor")
	first := usageTestRead(t, firstID, fixture.scope.Account, 12)
	second := usageTestRead(t, firstID, "second", 12)
	third := usageTestRead(t, otherID, "third", 12)
	legacy := SubscriptionQuota{Provider: "legacy-subscription", User: "legacy", Windows: []QuotaWindow{{Used: 12}}}
	warmSubscriptionUsageForTest(t, first, second, third, legacy)
	assertCards := func(cards []SubscriptionQuota, deferred ...bool) {
		t.Helper()
		for index, expected := range []SubscriptionQuota{first, second, third, legacy} {
			current := cardOf(cards, expected.Provider, expected.User)
			if current == nil || current.ReadDeferred != deferred[index] {
				t.Fatalf("card %s/%s = %+v, want deferred=%t", expected.Provider, expected.User, current, deferred[index])
			}
			if current.ReadDeferred && len(current.Windows) != 0 || !current.ReadDeferred && (len(current.Windows) != 1 || current.Windows[0].Used != 12) {
				t.Fatalf("card %s/%s has unexpected windows: %+v", expected.Provider, expected.User, current)
			}
		}
	}
	check := func(deferred ...bool) {
		t.Helper()
		assertCards(CachedCardsContext(t.Context(), time.Now()), deferred...)
	}
	check(false, false, false, false)
	if _, err := store.InvalidateUsage(second.extensionUsage.stamp.Scope); err != nil {
		t.Fatal(err)
	}
	check(false, true, false, false)
	if _, err := extensionhost.NewStore(fixture.manager.Namespace()).StateCAS(
		sdk.Scope{Plugin: fixture.scope.Plugin}, "module.config", 0, json.RawMessage(`{"changed":true}`),
	); err != nil {
		t.Fatal(err)
	}
	check(true, true, false, false)
	catalog := extensionhost.NewCatalog(fixture.manager.Namespace())
	for _, enabled := range []bool{false, true} {
		if err := catalog.SetEnabled(t.Context(), other.ID, enabled); err != nil {
			t.Fatal(err)
		}
	}
	check(true, true, true, false)
}

func TestQuotaSnapshotDefersAtStoreDeadline(t *testing.T) {
	fixture := keepAliveUsageFixture(t, "synthetic-allowed")
	id := ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider)
	card := usageTestRead(t, id, fixture.scope.Account, 12)
	unlock, ok := filelock.Lock(filepath.Join(fixture.manager.Namespace(), "private", ".lock"), time.Second)
	if !ok {
		t.Fatal("could not hold SDK Store writer")
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	kept := []SubscriptionQuota{card, {Provider: "key", Kind: "plan"}, {Provider: "remote", From: "elsewhere"}}
	checked := ValidateSubscriptionQuotas(ctx, kept)
	if !checked[0].ReadDeferred || checked[0].Error != context.DeadlineExceeded.Error() || len(checked[0].Windows) != 0 {
		t.Fatalf("held Store result = %+v", checked[0])
	}
	if kept[0].ReadDeferred || len(kept[0].Windows) != 1 || checked[1].ReadDeferred || checked[2].ReadDeferred {
		t.Fatal("validation modified the kept reading or deferred a key/remote card")
	}
}
