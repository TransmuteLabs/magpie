package extension

import (
	"encoding/json"
	"errors"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestModuleSettingsProjectionPinsOwner(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := componentFixtureManifest()
	manifest.Contract.Version.Minor = sdk.ModuleSettingsAPIMinor
	manifest.Settings = []sdk.FeatureSetting{{Name: "label", Title: "Label", Kind: sdk.FeatureSettingString, Default: "public"}}
	installed := installComponentFixture(t, manager, manifest)
	options, err := manager.ModuleSettings(t.Context(), installed)
	if err != nil || string(options.Values["label"]) != `"public"` || options.ConfigVersion != "0" {
		t.Fatalf("first settings snapshot: %+v, %v", options, err)
	}
	if err := manager.SetEnabled(t.Context(), manifest.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetEnabled(t.Context(), manifest.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ModuleSettings(t.Context(), installed); !errors.Is(err, ErrCatalogConflict) {
		t.Errorf("settings snapshot survived a change in enablement: %v", err)
	}
	entries, err := manager.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("current catalog: %+v, %v", entries, err)
	}
	selected := entries[0]
	manifest.Version = "2.0.0"
	manifest.Settings = nil
	installComponentFixture(t, manager, manifest)
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", json.RawMessage(`{"label":"private"}`)); err != nil {
		t.Fatal(err)
	}
	if projection, err := manager.ModuleSettings(t.Context(), selected); !errors.Is(err, ErrCatalogConflict) || len(projection.Values) != 0 {
		t.Fatalf("stale settings descriptor projected replacement configuration: %+v, %v", projection, err)
	}
}
