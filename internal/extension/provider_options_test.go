package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func installOptionsFixture(test *testing.T, manager *Manager) Installed {
	test.Helper()
	manifest := sdk.Manifest{Schema: 1, ID: "options.fixture", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 9}, Capabilities: []sdk.Capability{sdk.OutboundExecutor}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Vendor", Protocols: []protocolext.Protocol{"chat"},
			Options: []sdk.ProviderOption{
				{Key: "defaultOn", Label: "Default", Default: true},
				{Key: "paid", Label: "Paid", Confirm: "Uses account credits"},
			}}}}
	return installComponentFixture(test, manager, manifest)
}

func TestProviderOptionsUseCheckedRootConfigWithoutWorkers(test *testing.T) {
	manager := NewManager(test.TempDir())
	test.Cleanup(func() { _ = manager.Close(context.Background()) })
	entry := installOptionsFixture(test, manager)
	snapshot, err := manager.ProviderOptions(test.Context(), entry.Manifest.ID, "vendor")
	if err != nil || len(snapshot.Options) != 2 || !snapshot.Options[0].On || snapshot.Options[1].On || snapshot.ConfigVersion != "0" {
		test.Fatalf("declared defaults were not projected: %+v %v", snapshot, err)
	}
	if _, err := manager.CompareAndSwapModuleConfig(test.Context(), entry.Manifest.ID, "0",
		json.RawMessage(`{"unrelated":{"keep":1},"defaultOn":false}`)); err != nil {
		test.Fatal(err)
	}
	snapshot, err = manager.ProviderOptions(test.Context(), entry.Manifest.ID, "vendor")
	if err != nil || snapshot.Options[0].On || snapshot.ConfigVersion != "1" {
		test.Fatalf("explicit false was replaced by its default: %+v %v", snapshot, err)
	}
	if _, err := manager.CompareAndSwapProviderOptions(test.Context(), entry.Manifest.ID, "vendor", entry.Revision, "1",
		map[string]bool{"paid": true}, nil); err == nil {
		test.Fatal("a newly enabled paid switch bypassed confirmation")
	}
	config, err := manager.CompareAndSwapProviderOptions(test.Context(), entry.Manifest.ID, "vendor", entry.Revision, "1",
		map[string]bool{"paid": true}, []string{"paid"})
	if err != nil || config.Version != "2" || !bytes.Contains(config.Values, []byte(`"unrelated":{"keep":1}`)) ||
		!bytes.Contains(config.Values, []byte(`"defaultOn":false`)) {
		test.Fatalf("option save overwrote another configuration field: %+v %v", config, err)
	}
	for _, update := range []struct {
		provider, revision, version string
		values                      map[string]bool
	}{
		{"vendor", entry.Revision, "1", map[string]bool{"paid": false}},
		{"vendor", "stale", "2", map[string]bool{"paid": false}},
		{"vendor", entry.Revision, "2", map[string]bool{"undeclared": true}},
		{"other", entry.Revision, "2", map[string]bool{"paid": false}},
	} {
		if _, err := manager.CompareAndSwapProviderOptions(test.Context(), entry.Manifest.ID, update.provider,
			update.revision, update.version, update.values, nil); err == nil {
			test.Fatalf("stale or undeclared update was accepted: %+v", update)
		}
	}
	if _, err := manager.CompareAndSwapModuleConfig(test.Context(), entry.Manifest.ID, "2",
		json.RawMessage(`{"paid":"true"}`)); err == nil {
		test.Fatal("general configuration bypassed declared switch validation")
	}
	if err := manager.catalog.SetEnabled(test.Context(), entry.Manifest.ID, false); err != nil {
		test.Fatal(err)
	}
	if _, err := manager.CompareAndSwapProviderOptions(test.Context(), entry.Manifest.ID, "vendor", entry.Revision, "2",
		map[string]bool{"paid": false}, nil); err == nil {
		test.Fatal("a disabled provider accepted a stale editor update")
	}
	if manager.startCtx != nil || len(manager.workers) != 0 {
		test.Fatal("metadata and configuration started a plugin worker")
	}
}

func TestProviderOptionsRequireCurrentSubscriptionOwner(t *testing.T) {
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	entry := installOptionsFixture(t, manager)
	ctx := t.Context()
	owner := subscriptionRecord{
		SubscriptionOwner: SubscriptionOwner{
			Module: entry.Manifest.ID, Provider: "vendor", PublicID: "legacy",
			Revision: "owner-one", Active: true,
			Accounts: []SubscriptionAccount{{Key: "vendor", ID: "account", User: "account", LoginID: "account"}},
		},
		GrantRevision: strings.Repeat("a", 64),
		Digests:       map[string]string{"vendor": strings.Repeat("b", 64)},
		Tickets:       []string{"ticket"},
	}
	writeOwner := func() {
		t.Helper()
		if err := withTestSavedSignIns(manager.store, ctx, func(dir *os.Root) error {
			return storeWrite(dir, subscriptionOwnersFile, []subscriptionRecord{owner}, storeFileLimit)
		}); err != nil {
			t.Fatal(err)
		}
	}
	set := func(revision, version string, on bool) (sdk.ConfigSnapshot, error) {
		return manager.CompareAndSwapProviderOptions(WithSubscriptionOwnerRevision(ctx, revision),
			entry.Manifest.ID, "vendor", entry.Revision, version, map[string]bool{"defaultOn": on}, nil)
	}
	writeOwner()
	for _, revision := range []string{"", "another-owner"} {
		if _, err := set(revision, "0", false); !errors.Is(err, errSubscriptionChanged) {
			t.Fatalf("stale editor got %v for owner revision %q", err, revision)
		}
	}
	if config, err := set(owner.Revision, "0", false); err != nil || config.Version != "1" {
		t.Fatalf("current owner could not save the option: %+v %v", config, err)
	}
	previous := owner.Revision
	owner.Revision, owner.Active = "owner-two", false
	writeOwner()
	if _, err := set(previous, "1", true); !errors.Is(err, errSubscriptionChanged) {
		t.Fatalf("editor could write after rollback: %v", err)
	}
	config, err := manager.ModuleConfig(ctx, entry.Manifest.ID)
	if err != nil || config.Version != "1" || !bytes.Equal(config.Values, []byte(`{"defaultOn":false}`)) {
		t.Fatalf("a rejected update changed the configuration: %+v %v", config, err)
	}
	if _, err := manager.CompareAndSwapModuleConfig(WithSubscriptionOwnerRevision(ctx, previous),
		entry.Manifest.ID, "1", json.RawMessage(`{"defaultOn":true}`)); err != nil {
		t.Fatalf("module configuration was limited by a provider editor revision: %v", err)
	}
}

func TestProviderOptionsStopAtCatalogDeadline(t *testing.T) {
	manager := NewManager(t.TempDir())
	entry := installOptionsFixture(t, manager)
	root, err := manager.catalog.open(false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, action := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"read", func(ctx context.Context) error {
			_, err := manager.ProviderOptions(ctx, entry.Manifest.ID, "vendor")
			return err
		}},
		{"write", func(ctx context.Context) error {
			_, err := manager.CompareAndSwapProviderOptions(ctx, entry.Manifest.ID, "vendor", entry.Revision, "0",
				map[string]bool{"defaultOn": false}, nil)
			return err
		}},
	} {
		t.Run(action.name, func(t *testing.T) {
			unlock, err := catalogLock(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- action.run(ctx) }()
			select {
			case err := <-done:
				unlock()
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("provider option %s returned %v instead of the deadline", action.name, err)
				}
			case <-time.After(2 * time.Second):
				unlock()
				<-done
				t.Fatalf("provider option %s waited for the catalog after the deadline", action.name)
			}
		})
	}
	config, err := manager.ModuleConfig(t.Context(), entry.Manifest.ID)
	if err != nil || config.Version != "0" {
		t.Fatalf("canceled provider option write changed config: %+v %v", config, err)
	}
}
