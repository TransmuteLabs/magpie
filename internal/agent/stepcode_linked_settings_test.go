package agent

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func TestStepCodeSyncScopeRejectsConcurrentSettingsEdit(t *testing.T) {
	home := syncHome(t)
	wevnalSave(t, "kimi-k3", "glm-5.2")
	client := stepcode(home)
	catalog := filepath.Join(client.Dir, "models.json")
	const input = `# My StepCode settings
enabledModels  = [
  "magpie/wevnal/kimi-k3",
  "magpie/wevnal/glm-5.2:high",
]  # Scope
instructions = """
Keep every line.
"""
`
	writeFile(t, client.Path, input)
	if err := client.Apply("model", "magpie/wevnal/kimi-k3"); err != nil {
		t.Fatal(err)
	}
	before := readFile(catalog)
	stashBefore := readFile(stashPath())
	wevnalSave(t, "kimi-k3")
	current := readFile(client.Path)
	user := strings.Replace(current, `  "magpie/wevnal/kimi-k3",`, "  \"magpie/wevnal/kimi-k3\",\n  \"native/another\",", 1)
	if current == user {
		t.Fatalf("fixture has no multiline scope: %q", current)
	}
	target, err := edit.Target(client.Path)
	if err != nil {
		t.Fatal(err)
	}
	edited := false
	err = edit.Filtered(func(path string, data []byte) []byte {
		if path == target && !edited {
			writeFile(t, client.Path, user)
			edited = true
		}
		return data
	}, client.Sync)
	if !edited || !errors.Is(err, extension.ErrActionChanged) || readFile(client.Path) != user ||
		readFile(catalog) != before || readFile(stashPath()) != stashBefore {
		t.Fatalf("edited = %t, error = %v, settings = %q, catalog changed = %t, stash changed = %t",
			edited, err, readFile(client.Path), readFile(catalog) != before, readFile(stashPath()) != stashBefore)
	}
	if err := client.Sync(); err != nil {
		t.Fatal(err)
	}
	cfg, err := readStepcodeTOML(client.Path)
	gone := scopeGoneLoad(here(home).key("stepcode.scope.gone"))
	state, stateErr := loadStepcodeState(here(home))
	raw := readFile(client.Path)
	if err != nil || cfg.Scope == nil || !slices.Equal(*cfg.Scope, []string{"magpie/wevnal/kimi-k3", "native/another"}) ||
		!strings.Contains(raw, `enabledModels  = `) || !strings.Contains(raw, `]  # Scope`) ||
		!strings.Contains(raw, "instructions = \"\"\"\nKeep every line.\n\"\"\"\n") ||
		!slices.Equal(gone.Entries, []string{"magpie/wevnal/glm-5.2:high"}) ||
		stateErr != nil || state == nil || !slices.Equal(state.LastScope, []string{"magpie/wevnal/kimi-k3", "magpie/wevnal/glm-5.2:high"}) ||
		readFile(catalog) == before {
		t.Fatalf("retry error = %v, settings = %q, excluded = %+v, state = %+v (%v), catalog changed = %t",
			err, raw, gone, state, stateErr, readFile(catalog) != before)
	}
}

func TestStepCodeRenameScopeRejectsConcurrentSettingsEdit(t *testing.T) {
	home := syncHome(t)
	client := stepcode(home)
	const input = `defaultProvider = "native"
defaultModel = "first"
enabledModels = ["native/first", "magpie/relay/another:low"] # Scope
`
	writeFile(t, client.Path, input)
	if err := client.Apply("model", "magpie/relay/glm-4.6"); err != nil {
		t.Fatal(err)
	}
	key := here(home).key("stepcode.scope.gone")
	scopeGoneSave(key, scopeGone{Entries: []string{"magpie/relay/excluded:high"}})
	stashBefore := readFile(stashPath())
	user := readFile(client.Path) + "theme = \"light\"\n"
	target, err := edit.Target(client.Path)
	if err != nil {
		t.Fatal(err)
	}
	edited, moved := false, false
	err = edit.Filtered(func(path string, data []byte) []byte {
		if path == target && !edited {
			writeFile(t, client.Path, user)
			edited = true
		}
		return data
	}, func() error {
		var err error
		moved, err = client.RenameRefs("relay", "new")
		return err
	})
	if !edited || moved || !errors.Is(err, extension.ErrActionChanged) ||
		readFile(client.Path) != user || readFile(stashPath()) != stashBefore {
		t.Fatalf("edited = %t, moved = %t, error = %v, settings = %q, stash changed = %t",
			edited, moved, err, readFile(client.Path), readFile(stashPath()) != stashBefore)
	}
	if moved, err := client.RenameRefs("relay", "new"); err != nil || !moved {
		t.Fatalf("retry moved = %t, error = %v", moved, err)
	}
	cfg, err := readStepcodeTOML(client.Path)
	raw := readFile(client.Path)
	gone := scopeGoneLoad(key)
	state, stateErr := loadStepcodeState(here(home))
	want := []string{"native/first", "magpie/new/another:low", "magpie/new/glm-4.6"}
	if err != nil || cfg.Scope == nil || !slices.Equal(*cfg.Scope, want) || !strings.Contains(raw, `] # Scope`) ||
		!strings.Contains(raw, `theme = "light"`) || !slices.Equal(gone.Entries, []string{"magpie/new/excluded:high"}) ||
		stateErr != nil || state == nil || !slices.Equal(state.OrigScope, want[:2]) ||
		!slices.Equal(state.LastScope, want) || !slices.Equal(state.AppendedScope, want[2:]) {
		t.Fatalf("retry error = %v, settings = %q, excluded = %+v, state = %+v (%v)", err, raw, gone, state, stateErr)
	}
}

func TestStepCodeSyncScopeRejectsConcurrentCatalogEdit(t *testing.T) {
	home := syncHome(t)
	wevnalSave(t, "kimi-k3", "glm-5.2")
	client := stepcode(home)
	catalog := filepath.Join(client.Dir, "models.json")
	writeFile(t, client.Path, `enabledModels = ["magpie/wevnal/kimi-k3", "magpie/wevnal/glm-5.2"]`+"\n")
	if err := client.Apply("model", "magpie/wevnal/kimi-k3"); err != nil {
		t.Fatal(err)
	}
	settingsBefore := readFile(client.Path)
	stashBefore := readFile(stashPath())
	wevnalSave(t, "kimi-k3")
	user := readFile(catalog) + "\n// Other tool's edit\n"
	target, err := edit.Target(catalog)
	if err != nil {
		t.Fatal(err)
	}
	edited := false
	err = edit.Filtered(func(path string, data []byte) []byte {
		if path == target && !edited {
			writeFile(t, catalog, user)
			edited = true
		}
		return data
	}, client.Sync)
	if !edited || !errors.Is(err, extension.ErrActionChanged) || readFile(catalog) != user ||
		readFile(client.Path) != settingsBefore || readFile(stashPath()) != stashBefore {
		t.Fatalf("edited = %t, error = %v, catalog changed = %t, settings changed = %t, stash changed = %t",
			edited, err, readFile(catalog) != user, readFile(client.Path) != settingsBefore, readFile(stashPath()) != stashBefore)
	}
}

func TestStepCodeSyncRestoresEmptiedScopeWithNativeDefault(t *testing.T) {
	home := syncHome(t)
	wevnalSave(t, "kimi-k3", "glm-5.2")
	client := stepcode(home)
	writeFile(t, client.Path, `enabledModels = ["magpie/wevnal/glm-5.2:low"]`+"\n")
	if err := client.Apply("model", "magpie/wevnal/glm-5.2"); err != nil {
		t.Fatal(err)
	}
	const native = "defaultProvider = \"native\"\ndefaultModel = \"first\"\n"
	writeFile(t, client.Path, native+`enabledModels = ["magpie/wevnal/glm-5.2:low"]`+"\n")
	wevnalSave(t, "kimi-k3")
	if err := client.Sync(); err != nil {
		t.Fatal(err)
	}
	key := here(home).key("stepcode.scope.gone")
	gone := scopeGoneLoad(key)
	state, stateErr := loadStepcodeState(here(home))
	if readFile(client.Path) != native || !gone.Emptied || !slices.Equal(gone.Entries, []string{"magpie/wevnal/glm-5.2:low"}) ||
		stateErr != nil || state == nil || state.LastHasScope || len(state.LastScope) != 0 {
		t.Fatalf("settings = %q, excluded = %+v, state = %+v (%v)", readFile(client.Path), gone, state, stateErr)
	}
	wevnalSave(t, "kimi-k3", "glm-5.2")
	if err := client.Sync(); err != nil {
		t.Fatal(err)
	}
	cfg, err := readStepcodeTOML(client.Path)
	gone = scopeGoneLoad(key)
	state, stateErr = loadStepcodeState(here(home))
	want := []string{"magpie/wevnal/glm-5.2:low", "native/first"}
	if err != nil || cfg.Scope == nil || !slices.Equal(*cfg.Scope, want) || client.Field("model").Get() != "native/first" ||
		len(gone.Entries) != 0 || gone.Emptied || stateErr != nil || state == nil ||
		!state.LastHasScope || !slices.Equal(state.LastScope, want) {
		t.Fatalf("settings = %q (%v), excluded = %+v, state = %+v (%v)", readFile(client.Path), err, gone, state, stateErr)
	}
}

func TestStepCodeSyncLeavesMalformedAndExplicitEmptyScope(t *testing.T) {
	for _, test := range []struct {
		name string
		toml string
	}{
		{"malformed", "enabledModels = ["},
		{"wrong type", "enabledModels = true\n"},
		{"empty", "enabledModels = [] # No filter\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := syncHome(t)
			wevnalSave(t, "kimi-k3", "glm-5.2")
			client := stepcode(home)
			if err := client.Apply("model", "magpie/wevnal/kimi-k3"); err != nil {
				t.Fatal(err)
			}
			writeFile(t, client.Path, test.toml)
			key := here(home).key("stepcode.scope.gone")
			scopeGoneSave(key, scopeGone{Entries: []string{"magpie/wevnal/kimi-k3"}, Emptied: true})
			stashBefore := readFile(stashPath())
			wevnalSave(t, "kimi-k3")
			if err := client.Sync(); err != nil {
				t.Fatal(err)
			}
			catalog := readFile(filepath.Join(client.Dir, "models.json"))
			if readFile(client.Path) != test.toml || readFile(stashPath()) != stashBefore ||
				!strings.Contains(catalog, "wevnal/kimi-k3") || strings.Contains(catalog, "wevnal/glm-5.2") {
				t.Fatalf("settings = %q, stash changed = %t, catalog = %s", readFile(client.Path), readFile(stashPath()) != stashBefore, catalog)
			}
		})
	}
}
