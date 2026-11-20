package library

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/extension"
)

func TestCoreAgentLibraryUsesSelectedConfiguration(t *testing.T) {
	home := sandbox(t)
	entries, err := extension.Default().List()
	if err != nil || len(entries) != 0 {
		t.Fatalf("expected an empty installed SDK catalog, got %d entries: %v", len(entries), err)
	}
	for _, fixture := range []struct {
		id, variable, config, instructions, mcp, override, settings string
	}{
		{"codex", "CODEX_HOME", "config.toml", "AGENTS.md", "config.toml", "AGENTS.override.md", ""},
		{"claude", "CLAUDE_CONFIG_DIR", "settings.json", "CLAUDE.md", ".claude.json", "", "settings.json"},
	} {
		t.Run(fixture.id, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "custom "+fixture.id)
			t.Setenv(fixture.variable, dir)
			write(t, filepath.Join(dir, fixture.config), "")
			ordinary, err := agent.Find(fixture.id)
			if err != nil {
				t.Fatal(err)
			}
			selected, places, owned := ordinary.SDKLibraryTarget()
			if !owned || selected != ordinary || places == nil {
				t.Fatal("ordinary agent does not contribute its Library capability through SDK")
			}
			target := targetByID(fixture.id)
			if target == nil || target.MCP == nil || target.Instructions != filepath.Join(dir, fixture.instructions) ||
				target.MCP.Path != filepath.Join(dir, fixture.mcp) || target.Skills != filepath.Join(dir, "skills") {
				t.Fatalf("Library did not select the agent's configured directory: %+v", target)
			}
			path := func(name string) string {
				if name == "" {
					return ""
				}
				return filepath.Join(dir, name)
			}
			if target.Override != path(fixture.override) || target.Settings != path(fixture.settings) {
				t.Fatalf("override/settings were lost: %+v", target)
			}
			if targetOf(&agent.Agent{ID: fixture.id, Dir: dir}) != nil {
				t.Fatal("Library dispatches by agent ID without a contributed capability")
			}
			write(t, target.Instructions, "Personal guidance.\n")
			id, err := Takes(fixture.id, "instructions")
			if err != nil {
				t.Fatal(err)
			}
			shared := "Shared guidance."
			ok(t)(SaveInstructions(InstructionsChange{Shared: &shared, Agents: []string{id}}))
			if got := read(t, target.Instructions); !strings.Contains(got, shared) || !strings.Contains(got, "Personal guidance.") {
				t.Fatalf("ordinary Library write lost the selected resource: %q", got)
			}
			ok(t)(SaveInstructions(InstructionsChange{Agents: []string{}}))
			if got := read(t, target.Instructions); got != "Personal guidance.\n" {
				t.Fatalf("ordinary removal changed user content: %q", got)
			}
			catalog, err := extension.Default().List()
			if err != nil || len(catalog) != 0 {
				t.Fatal("linked capabilities changed the installed package catalog", err)
			}
		})
	}
	if got := read(t, filepath.Join(home, ".codex", "config.toml")); got != "" {
		t.Fatalf("default Codex configuration changed: %q", got)
	}
}
