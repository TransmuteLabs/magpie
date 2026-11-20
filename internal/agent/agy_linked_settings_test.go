package agent

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func TestAgySyncKeepsLegacySettingsWhenOverlayFails(t *testing.T) {
	home, _ := codexHome(t, "", "")
	agent := agy(home)
	settings := agent.Path
	overlay := filepath.Join(agent.Dir, agyOverride)
	const original = `{"model":"magpie/fake/m1","modelProvider":"gemini","trustedWorkspaces":["/mine"]}`
	const malformed = `{"broken": [`
	writeFile(t, settings, original)
	writeFile(t, overlay, malformed)
	modelClaim, providerClaim := "agy:"+settings+":model", "agy:"+settings+":modelProvider"
	stash(map[string]string{modelClaim: "my-model", providerClaim: "vertex"})

	if err := agent.Sync(); err == nil {
		t.Fatal("Sync accepted a malformed overlay")
	}
	if readFile(settings) != original || readFile(overlay) != malformed {
		t.Fatal("failed Sync changed one of the settings files")
	}
	if saved := peek(modelClaim, providerClaim); saved[modelClaim] != "my-model" || saved[providerClaim] != "vertex" {
		t.Fatalf("failed Sync consumed restoration state: %v", saved)
	}

	writeFile(t, overlay, `{}`)
	if err := agent.Sync(); err != nil {
		t.Fatal(err)
	}
	if model, _ := edit.GetJSON(settings, "model"); model != "my-model" {
		t.Fatalf("retry restored %q", model)
	}
	if model, _ := edit.GetJSON(overlay, "model"); model != "magpie/fake/m1" {
		t.Fatalf("retry routed %q", model)
	}
}

func TestAgySyncPreservesConcurrentNativeSettingsEdit(t *testing.T) {
	home, _ := codexHome(t, "", "")
	agent := agy(home)
	settings := agent.Path
	overlay := filepath.Join(agent.Dir, agyOverride)
	const previousOverlay = `{"model":"magpie/fake/m1"}`
	const changed = `{"model":"mine","customModelsConfig":{"customModels":{"new":{"modelName":"user-model"}}}}`
	writeFile(t, settings, `{"model":"mine"}`)
	writeFile(t, overlay, previousOverlay)
	target, err := edit.Target(overlay)
	if err != nil {
		t.Fatal(err)
	}
	edited := false
	err = edit.Filtered(func(path string, next []byte) []byte {
		if path == target && !edited {
			edited = true
			writeFile(t, settings, changed)
		}
		return next
	}, agent.Sync)
	if !edited || !errors.Is(err, extension.ErrActionChanged) {
		t.Fatalf("Sync = %v, native edit injected = %t", err, edited)
	}
	if readFile(settings) != changed || readFile(overlay) != previousOverlay {
		t.Fatal("conflicting Sync changed native or overlay settings")
	}
	if err := agent.Sync(); err != nil {
		t.Fatal(err)
	}
	if model, _ := edit.GetJSON(overlay, "customModelsConfig.customModels.new.modelName"); model != "user-model" {
		t.Fatalf("retry lost the user's custom model: %q", model)
	}
}
