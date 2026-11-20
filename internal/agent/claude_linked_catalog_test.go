package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
)

func TestClaudeModelPickPublishesCatalogWithRoute(t *testing.T) {
	home, path := desktopWindowSandbox(t)
	agent := claude(home)
	target, err := edit.Target(path)
	if err != nil {
		t.Fatal(err)
	}
	var earlyCatalog, sawRoute, completeRoute bool
	err = edit.Filtered(func(name string, proposed []byte) []byte {
		if name != target {
			return proposed
		}
		body := string(proposed)
		picker := strings.Contains(body, `"modelPicker"`)
		caps := strings.Contains(body, claudeCapsEnv)
		if !strings.Contains(body, `"ANTHROPIC_BASE_URL"`) {
			earlyCatalog = earlyCatalog || picker || caps
		} else if !sawRoute {
			sawRoute = true
			completeRoute = picker && caps
		}
		return proposed
	}, func() error { return agent.Apply("model", "v/gpt") })
	if err != nil || earlyCatalog || !completeRoute {
		t.Fatalf("Apply = %v, catalog published before route = %t, route contained picker and capabilities = %t", err, earlyCatalog, completeRoute)
	}
}

func TestClaudeSyncPreservesConcurrentCatalogEdit(t *testing.T) {
	home, path := desktopWindowSandbox(t)
	agent := claude(home)
	if err := agent.Apply("model", "v/gpt"); err != nil {
		t.Fatal(err)
	}
	before := stashLoad()
	if err := provider.SetModelEfforts("v/gpt", []string{"high", "xhigh", "max"}); err != nil {
		t.Fatal(err)
	}
	vendor, err := provider.Find("v")
	if err != nil {
		t.Fatal(err)
	}
	vendor.Models = append(vendor.Models, "added-after-connect")
	if err := provider.Save(*vendor); err != nil {
		t.Fatal(err)
	}
	target, err := edit.Target(path)
	if err != nil {
		t.Fatal(err)
	}
	var external string
	err = edit.Filtered(func(name string, proposed []byte) []byte {
		if name == target && external == "" && strings.Contains(string(proposed), "added-after-connect") {
			body, err := edit.PatchJSON([]byte(readFile(name)), []edit.KV{
				{Path: "modelPicker", Value: map[string]any{"options": []map[string]string{
					{"model": "user-picked", "label": "User model", "description": "my picker"},
				}}},
				{Path: "env." + claudeCapsEnv, Value: "user-picked=effort"},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			external = string(body)
			writeFile(t, name, external)
		}
		return proposed
	}, agent.Sync)
	if external == "" || !errors.Is(err, extension.ErrActionChanged) || readFile(path) != external {
		t.Errorf("Sync = %v, concurrent settings preserved = %t", err, external != "" && readFile(path) == external)
	}
	after := stashLoad()
	for _, key := range []string{"claude.model_picker", "claude.capabilities"} {
		if after[key] != before[key] {
			t.Errorf("rejected Sync changed %s", key)
		}
	}
	if t.Failed() {
		return
	}
	if err := agent.Sync(); err != nil {
		t.Fatal(err)
	}
	if readFile(path) != external {
		t.Fatal("retry overwrote the user's catalog settings")
	}
}

func TestClaudeDisconnectPreservesEditedPicker(t *testing.T) {
	home, path := desktopWindowSandbox(t)
	agent := claude(home)
	if err := agent.Apply("model", "v/gpt"); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetJSON(path, edit.KV{Path: "modelPicker.options.0.label", Value: "My label"}); err != nil {
		t.Fatal(err)
	}
	want, ok := edit.GetJSON(path, "modelPicker")
	if !ok || !strings.Contains(want, "My label") {
		t.Fatalf("test picker edit missing: %q", want)
	}
	if err := agent.Sync(); err != nil {
		t.Fatal(err)
	}
	if got, _ := edit.GetJSON(path, "modelPicker"); got != want {
		t.Error("Sync overwrote the edited picker")
	}
	if err := agent.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if got, _ := edit.GetJSON(path, "modelPicker"); got != want {
		t.Error("Disconnect removed the edited picker")
	}
	if _, has := edit.GetJSON(path, "env."+claudeCapsEnv); has {
		t.Error("Disconnect kept the capabilities Magpie wrote")
	}
}
