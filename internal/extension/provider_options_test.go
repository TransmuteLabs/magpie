package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

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
