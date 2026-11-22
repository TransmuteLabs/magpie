package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSessionMigrationSelectionKeepsSDK10Identity(t *testing.T) {
	manager := extension.NewManager(t.TempDir())
	t.Cleanup(func() { manager.Close(context.Background()) })
	manifest := sdk.Manifest{Schema: 1, ID: "fixture.sessions", Version: "1.0.0", Runtime: "exec", Entry: "worker.exe",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1}, Capabilities: []sdk.Capability{sdk.SessionMigration}},
		Sessions: []sdk.SessionDescriptor{{ID: "conversation", Name: "Conversation", From: "claude", To: "omp", OutputDir: ".omp/agent/sessions"}}}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, manifest.Entry), []byte("selection must not launch the worker"), 0o700); err != nil {
		t.Fatal(err)
	}
	installed, err := extension.NewCatalog(manager.Namespace()).Install(t.Context(), directory, func(context.Context, extension.Installed) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manager.Namespace(), "private"), []byte("SDK 1.0 has no module configuration store"), 0o600); err != nil {
		t.Fatal(err)
	}

	selected, err := sessionMigrationSelection(t.Context(), manager, manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal([]string{manager.Namespace(), manifest.ID, installed.Revision, ""})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(legacy)
	if selected.entry.Revision != installed.Revision || selected.configVersion != "" || selected.identity != hex.EncodeToString(digest[:]) {
		t.Fatalf("SDK 1.0 selection changed its legacy identity or config version: %+v", selected)
	}
}

func TestWindowStorageSelectionRejectsChangedModuleAuthority(t *testing.T) {
	manager, owner := windowStorageFixture(t)
	first, err := InspectWindowStorage(t.Context(), owner, "history")
	if err != nil || len(first.Roots) != 2 {
		t.Fatalf("initial storage selection: %+v, %v", first, err)
	}
	cycleSourceModule(t, manager, owner.Module, "")
	stale := RequestWindowStorage(t.Context(), owner, "history", first.ViewRef, first.Roots[0].RootRef)
	if stale.State != "changed" {
		t.Fatalf("stale pre-cycle view was accepted: %+v", stale)
	}
	fresh, err := InspectWindowStorage(t.Context(), owner, "history")
	if err != nil || len(fresh.Roots) != 2 || fresh.ViewRef == first.ViewRef || fresh.Roots[0].Consented {
		t.Fatalf("fresh storage selection after cycle: %+v, %v", fresh, err)
	}

	peer := extension.NewManager(manager.Namespace())
	t.Cleanup(func() { peer.Close(context.Background()) })
	config, err := peer.CompareAndSwapModuleConfig(t.Context(), owner.Module, owner.ConfigVersion, json.RawMessage(`{"title":"new"}`))
	if err != nil || config.Version == owner.ConfigVersion {
		t.Fatalf("change module configuration: %+v, %v", config, err)
	}
	if _, err := InspectWindowStorage(t.Context(), owner, "history"); !errors.Is(err, extension.ErrActionChanged) {
		t.Fatalf("pre-change config was accepted: %v", err)
	}
	owner.ConfigVersion = config.Version
	current, err := InspectWindowStorage(t.Context(), owner, "history")
	if err != nil || len(current.Roots) != 2 || current.ViewRef == fresh.ViewRef {
		t.Fatalf("select with current config: %+v, %v", current, err)
	}
}
