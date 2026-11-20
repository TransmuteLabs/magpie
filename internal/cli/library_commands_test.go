package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/library"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestLibrarySDKCommandImport(t *testing.T) {
	cliHome(t)
	home := os.Getenv("HOME")
	t.Setenv("PATH", filepath.Join(home, "bin"))
	testenv.Program(t, filepath.Join(home, "bin", "claude"), "#!/bin/sh\n")
	claude := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(claude, "commands"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	const (
		moduleID = "commands-fixture"
		sourceID = moduleID + "/review"
	)
	catalog := extension.NewCatalog(extension.Default().Namespace())
	install := func(version, prompt string) {
		t.Helper()
		source := t.TempDir()
		manifest := map[string]any{
			"schema": 1, "id": moduleID, "version": version, "runtime": "exec", "entry": "worker",
			"contract": map[string]any{
				"version": map[string]int{"major": 1, "minor": 18}, "capabilities": []string{"module-rpc"},
			},
			"contributions": []map[string]any{
				{"point": "library-command", "version": 1, "libraryCommand": map[string]string{
					"id": "review", "description": "  Review   changes ", "prompt": prompt,
				}},
				{"point": "library-rule", "version": 1, "libraryRule": map[string]string{
					"id": "review", "body": "A separate rule",
				}},
			},
		}
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "worker"), []byte("cannot run"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.Install(t.Context(), source, func(context.Context, extension.Installed) error {
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	type selection struct {
		Name     string `json:"name"`
		Revision string `json:"revision"`
		Prompt   string `json:"prompt"`
	}
	list := func(kind string) []selection {
		t.Helper()
		output, err := runOut(t, "library", kind, "available")
		var selections []selection
		if err != nil || json.Unmarshal([]byte(output), &selections) != nil || selections == nil {
			t.Fatalf("%s available: %s (%v)", kind, output, err)
		}
		return selections
	}
	selectCommand := func(prompt string) selection {
		t.Helper()
		selections := list("commands")
		if len(selections) != 1 || selections[0].Name != sourceID ||
			selections[0].Prompt != prompt || len(selections[0].Revision) != 64 {
			t.Fatalf("unexpected command selections: %+v", selections)
		}
		return selections[0]
	}
	importCommand := func(choice selection, name, agents string) error {
		t.Helper()
		_, err := runOut(t, "library", "commands", "import", choice.Name, name, agents, "--expected", choice.Revision)
		return err
	}
	path := func(name string) string {
		return filepath.Join(claude, "commands", name+".md")
	}
	expectAbsent := func(name string) {
		t.Helper()
		if _, err := os.Stat(path(name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s: unexpected command file: %v", name, err)
		}
	}

	install("1.0.0", "Original $1")
	first := selectCommand("Original $1")
	expectAbsent("review")
	if _, err := runOut(t, "plugin", "off", moduleID); err != nil {
		t.Fatal(err)
	}
	if selections := list("commands"); len(selections) != 0 {
		t.Fatalf("disabled module offered commands: %+v", selections)
	}
	if err := importCommand(first, "review", "claude"); err == nil {
		t.Fatal("disabled module imported a command")
	}
	if _, err := runOut(t, "plugin", "on", moduleID); err != nil {
		t.Fatal(err)
	}
	if err := importCommand(first, "review", "claude"); err == nil {
		t.Fatal("selection remained valid after disable and enable")
	}
	first = selectCommand("Original $1")
	const prompt = "Revised $1\r\nSecond line  "
	install("2.0.0", prompt)
	if err := importCommand(first, "review", "claude"); err == nil {
		t.Fatal("selection remained valid after replacement")
	}
	choice := selectCommand(prompt)
	rules := list("rules")
	if len(rules) != 1 || rules[0].Name != sourceID {
		t.Fatalf("unexpected rule selections: %+v", rules)
	}
	if err := importCommand(rules[0], "review", "claude"); err == nil {
		t.Fatal("accepted a rule's revision for a command with the same ID")
	}
	if err := importCommand(choice, "review", "unknown-agent"); err == nil {
		t.Fatal("accepted an unsupported agent")
	}
	if err := importCommand(choice, "../outside", "claude"); err == nil {
		t.Fatal("accepted a name outside the command destination")
	}
	if err := importCommand(choice, "review", " "); err == nil {
		t.Fatal("accepted missing agent selection")
	}
	expectAbsent("review")
	if err := importCommand(choice, "unassigned", "none"); err != nil {
		t.Fatal(err)
	}
	expectAbsent("unassigned")
	if err := importCommand(choice, "review", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := importCommand(choice, "review", "claude"); err == nil || !strings.Contains(err.Error(), "already has a slash command") {
		t.Fatalf("duplicate import: %v", err)
	}
	rendered, err := os.ReadFile(path("review"))
	if err != nil || !strings.Contains(string(rendered), "description:") ||
		!strings.Contains(string(rendered), "Review changes") ||
		!strings.HasSuffix(string(rendered), "\nRevised $ARGUMENTS[0]\nSecond line\n") {
		t.Fatalf("agent's saved command: %q (%v)", rendered, err)
	}
	if err := os.WriteFile(path("owned-by-user"), rendered, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := importCommand(choice, "owned-by-user", "claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := runOut(t, "plugin", "rm", moduleID); err != nil {
		t.Fatal(err)
	}
	if selections := list("commands"); len(selections) != 0 {
		t.Fatalf("removed module offered commands: %+v", selections)
	}
	if err := importCommand(choice, "removed", "claude"); err == nil {
		t.Fatal("removed module was still importable")
	}
	expectAbsent("removed")
	if data, err := os.ReadFile(path("review")); err != nil || string(data) != string(rendered) {
		t.Fatalf("removing module changed the Library copy: %q (%v)", data, err)
	}
	if _, err := library.RemoveCommands([]string{"review", "owned-by-user", "unassigned"}); err != nil {
		t.Fatal(err)
	}
	expectAbsent("review")
	if data, err := os.ReadFile(path("owned-by-user")); err != nil || string(data) != string(rendered) {
		t.Fatalf("removing Library command changed user's file: %q (%v)", data, err)
	}
}
