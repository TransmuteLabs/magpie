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

func TestCodexDisconnectPreservesModelEditedDuringRestore(t *testing.T) {
	home, read := codexHome(t, "", "model = \"gpt-5.5\"\n")
	cx := codex(home)
	if err := cx.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	target, err := edit.Target(cx.Path)
	if err != nil {
		t.Fatal(err)
	}
	const routed = `model = "fake/m1"`
	const restored = `model = "gpt-5.5"`
	const external = `model = "personal"`
	var user string
	err = edit.Filtered(func(path string, proposed []byte) []byte {
		if path == target && user == "" && strings.Contains(string(proposed), restored) {
			current := read()
			if !strings.Contains(current, routed) {
				t.Fatalf("restore started without the routed model: %s", current)
			}
			user = strings.Replace(current, routed, external, 1)
			writeFile(t, path, user)
		}
		return proposed
	}, cx.Disconnect)
	if user == "" || !errors.Is(err, extension.ErrActionChanged) || read() != user {
		t.Fatalf("Disconnect lost a concurrent model edit: injected=%t error=%v config=%q", user != "", err, read())
	}
	if got := stashLoad()["codex.model"]; got != "gpt-5.5" {
		t.Fatalf("Disconnect lost the saved model after rejection: %q", got)
	}
}

func TestCodexDisconnectKeepsAuxiliaryModelsWhenRestoreRejects(t *testing.T) {
	home, read := codexHome(t, "", "model = \"gpt-5.5\"\n")
	cx := codex(home)
	if err := cx.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	if err := cx.Field("memories").Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	target, err := edit.Target(cx.Path)
	if err != nil {
		t.Fatal(err)
	}
	const auxiliary = "\n[agents]\n'default_subagent_model' = '''\nfake/m1''' # routed pick\n"
	const independent = `"consolidation_model" = 'own-memory-model' # keep`
	routed := strings.Replace(read(), `consolidation_model = "fake/m1"`, independent, 1) + auxiliary
	if !strings.Contains(routed, independent) || !strings.Contains(routed, `extract_model = "fake/m1"`) {
		t.Fatalf("missing memory picks in setup:\n%s", routed)
	}
	writeFile(t, target, routed)

	const lateEdit = "\n# saved by the user during Disconnect\n"
	var user string
	err = edit.Filtered(func(path string, proposed []byte) []byte {
		if path == target && user == "" && strings.Contains(string(proposed), `model = "gpt-5.5"`) {
			user = read() + lateEdit
			writeFile(t, path, user)
		}
		return proposed
	}, cx.Disconnect)
	if user == "" || !errors.Is(err, extension.ErrActionChanged) {
		t.Fatalf("Disconnect did not reject a stale restore: injected=%t error=%v", user != "", err)
	}
	if got := read(); got != routed+lateEdit {
		t.Fatalf("rejected Disconnect changed the routed config before restoration:\n%s", got)
	}
	if got := stashLoad()["codex.model"]; got != "gpt-5.5" {
		t.Fatalf("Disconnect lost the saved model after rejection: %q", got)
	}

	if err := cx.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if got := read(); strings.Contains(got, "fake/m1") || !strings.Contains(got, independent) ||
		!strings.Contains(got, `model = "gpt-5.5"`) || !strings.HasSuffix(got, lateEdit) {
		t.Fatalf("retry failed to restore the original model and preserve the independent edits:\n%s", got)
	}
}

func TestCodexDisconnectKeepsProviderURLsWhenRestoreRejects(t *testing.T) {
	const mirror = "\n[model_providers.cc-switch-3]\nname = \"OpenAI\"\nrequires_openai_auth = true\nwire_api = \"responses\"\n"
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, ccSwitchCodex+mirror)
	cx := codex(home)
	if err := cx.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	target, err := edit.Target(cx.Path)
	if err != nil {
		t.Fatal(err)
	}
	at := here(home)
	keys := []string{at.key("codex.tables"), at.key("codex.mirrors")}
	saved := peek(keys...)
	for _, key := range keys {
		if saved[key] == "" {
			t.Fatalf("missing saved provider URL ownership %q", key)
		}
	}
	routed := read()
	const lateEdit = "\n# saved by CC Switch during Disconnect\n"
	var injected bool
	err = edit.Filtered(func(path string, proposed []byte) []byte {
		if path == target && !injected && strings.Contains(string(proposed), `model = "gpt-5.4"`) {
			injected = true
			writeFile(t, path, read()+lateEdit)
		}
		return proposed
	}, cx.Disconnect)
	if !injected || !errors.Is(err, extension.ErrActionChanged) {
		t.Fatalf("Disconnect did not reject a stale restore: injected=%t error=%v", injected, err)
	}
	if got := read(); got != routed+lateEdit {
		t.Errorf("rejected Disconnect changed provider URLs:\n%s", got)
	}
	for _, key := range keys {
		if got := stashLoad()[key]; got != saved[key] {
			t.Errorf("rejected Disconnect forgot %q: got %q, want %q", key, got, saved[key])
		}
	}
	if t.Failed() {
		return
	}

	if err := cx.Disconnect(); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"custom": "https://relay.example/v1", "cc-switch-3": "", "cc-switch-2": "https://old.example/v1", "mine": "https://mine.example/v1",
	} {
		table, err := edit.GetTOMLTable(target, "model_providers."+id)
		if err != nil || table["base_url"] != want {
			t.Errorf("retry did not restore %q: got %q, want %q, error=%v", id, table["base_url"], want, err)
		}
	}
	for _, key := range keys {
		if got := stashLoad()[key]; got != "" {
			t.Errorf("successful Disconnect kept %q: %q", key, got)
		}
	}
	if got := read(); strings.Contains(got, "fake/m1") || !strings.Contains(got, `model_provider = "custom"`) ||
		!strings.Contains(got, `model = "gpt-5.4"`) || !strings.HasSuffix(got, lateEdit) {
		t.Fatalf("retry did not restore the original route and preserve the user's edit:\n%s", got)
	}
}

func quoteTOML(s string) string { return "\"" + strings.ReplaceAll(s, `\`, `\\`) + "\"" }
