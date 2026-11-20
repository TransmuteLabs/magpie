package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

// Disconnect puts back only what magpie changed: a key the user edited by
// hand while Codex was on magpie keeps the user's value, though magpie had
// saved what was there before it connected (yhwangl on X).
func TestCodexDisconnectKeepsHandEdits(t *testing.T) {
	home, read := codexHome(t, "", "model = \"gpt-5.5\"\nmodel_reasoning_effort = \"low\"\nmodel_catalog_json = \"/x/old-list.json\"\n")
	theirs := filepath.Join(home, "my-list.json")
	os.WriteFile(theirs, []byte(`{"models":[]}`), 0o644)
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	cfg := read()
	if !strings.Contains(cfg, "model_provider = \"magpie\"") {
		t.Fatalf("not magpie's provider:\n%s", cfg)
	}
	// by hand, with magpie connected: an effort and a model list of their own
	cfg = strings.Replace(cfg, "model_reasoning_effort = \"low\"", "model_reasoning_effort = \"high\"", 1)
	lines := strings.Split(cfg, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "model_catalog_json") {
			lines[i] = "model_catalog_json = " + quoteTOML(theirs)
		}
	}
	os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(strings.Join(lines, "\n")), 0o644)
	if err := codex(home).Disconnect(); err != nil {
		t.Fatal(err)
	}
	cfg = read()
	for _, want := range []string{"model_reasoning_effort = \"high\"\n", "model_catalog_json = " + quoteTOML(theirs) + "\n", "model = \"gpt-5.5\"\n"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("missing %q after Disconnect:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "model_provider = \"magpie\"") {
		t.Errorf("still on magpie:\n%s", cfg)
	}
}

// What magpie changed and the user didn't is put back as it was.
func TestCodexDisconnectPutsBackWhatMagpieChanged(t *testing.T) {
	home, read := codexHome(t, "", "model = \"gpt-5.5\"\nmodel_reasoning_effort = \"low\"\n")
	if err := codex(home).Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	if err := codex(home).Disconnect(); err != nil {
		t.Fatal(err)
	}
	cfg := read()
	for _, want := range []string{"model_reasoning_effort = \"low\"\n", "model = \"gpt-5.5\"\n"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("missing %q after Disconnect:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "model_catalog_json") || strings.Contains(cfg, "model_provider =") {
		t.Errorf("magpie's keys left:\n%s", cfg)
	}
}

func TestCodexDisconnectPreservesEditDuringRestore(t *testing.T) {
	home, read := codexHome(t, "", "model = \"gpt-5.5\"\nmodel_reasoning_effort = \"low\"\n")
	cx := codex(home)
	if err := cx.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	if err := cx.Apply("effort", "medium"); err != nil {
		t.Fatal(err)
	}
	target, err := edit.Target(cx.Path)
	if err != nil {
		t.Fatal(err)
	}
	const before = `model_reasoning_effort = "medium"`
	const restored = `model_reasoning_effort = "low"`
	const external = `model_reasoning_effort = "xhigh"`
	var user string
	err = edit.Filtered(func(path string, proposed []byte) []byte {
		if path == target && user == "" && strings.Contains(string(proposed), restored) {
			current := read()
			if !strings.Contains(current, before) {
				t.Fatalf("restore started without the applied effort: %s", current)
			}
			user = strings.Replace(current, before, external, 1)
			writeFile(t, path, user)
		}
		return proposed
	}, cx.Disconnect)
	if user == "" || !errors.Is(err, extension.ErrActionChanged) {
		t.Fatalf("Disconnect did not reject a stale restore: injected=%t error=%v", user != "", err)
	}
	if got := read(); got != user {
		t.Fatalf("Disconnect changed the user's concurrent config:\n%s", got)
	}
	if got := stashLoad()["codex.effort"]; got != "low" {
		t.Fatalf("Disconnect lost the saved effort after rejection: %q", got)
	}
}

func quoteTOML(s string) string { return "\"" + strings.ReplaceAll(s, `\`, `\\`) + "\"" }
