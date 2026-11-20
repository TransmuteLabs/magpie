package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarkdownOutputsRequireLibraryOwnership(t *testing.T) {
	for _, owner := range []struct {
		kind, folder string
		save         func() (*Result, error)
		remove       func() (*Result, error)
		claims       func(*Applied) []SubagentFile
	}{
		{"rule", "rules", func() (*Result, error) {
			return SaveRule("", Rule{Name: "keeper", Body: "Keep this.", Agents: []string{"claude"}})
		}, func() (*Result, error) { return RemoveRules([]string{"keeper"}) }, func(a *Applied) []SubagentFile { return a.Rules }},
		{"command", "commands", func() (*Result, error) {
			return SaveCommand("", Command{Name: "keeper", Prompt: "Keep this.", Agents: []string{"claude"}})
		}, func() (*Result, error) { return RemoveCommands([]string{"keeper"}) }, func(a *Applied) []SubagentFile { return a.Commands }},
		{"subagent", "agents", func() (*Result, error) {
			return SaveSubagent("", Subagent{Name: "keeper", Description: "Keep this.", Prompt: "Keep this.", Agents: []string{"claude"}})
		}, func() (*Result, error) { return RemoveSubagents([]string{"keeper"}) }, func(a *Applied) []SubagentFile { return a.Subagents }},
	} {
		t.Run(owner.kind, func(t *testing.T) {
			for _, state := range []string{"existing-equal", "existing-symlink", "dangling-symlink", "replaced-by-symlink"} {
				t.Run(state, func(t *testing.T) {
					home := sandbox(t)
					output := filepath.Join(home, ".claude", owner.folder, "keeper.md")
					source := filepath.Join(home, "dotfiles", "keeper.md")
					ok(t)(owner.save())
					body := read(t, output)
					if state == "replaced-by-symlink" {
						if err := os.Remove(output); err != nil {
							t.Fatal(err)
						}
					} else {
						ok(t)(owner.remove())
						if _, err := os.Lstat(output); !os.IsNotExist(err) {
							t.Fatalf("Library-created file survived removal: %v", err)
						}
					}

					if state == "existing-equal" {
						write(t, output, body)
					} else {
						if state != "dangling-symlink" {
							write(t, source, body)
						}
						symlink(t, source, output)
					}
					if state != "replaced-by-symlink" {
						result, err := owner.save()
						expectMarkdownOwnershipConflict(t, output, result, err)
					}
					result, err := Sync()
					expectMarkdownOwnershipConflict(t, output, result, err)
					if applied := loaded(t).Applied["claude"]; applied != nil && len(owner.claims(applied)) != 0 {
						t.Errorf("Library claimed the user's %s", state)
					}
					ok(t)(owner.remove())
					if state == "existing-equal" {
						if contents, err := os.ReadFile(output); err != nil || string(contents) != body {
							t.Fatalf("user-owned regular file changed or disappeared: %q, %v", contents, err)
						}
						return
					}
					if destination, err := os.Readlink(output); err != nil || destination != source {
						t.Errorf("user-owned symlink changed or disappeared: %q, %v", destination, err)
					}
					if state == "dangling-symlink" {
						if _, err := os.Lstat(source); !os.IsNotExist(err) {
							t.Errorf("Library wrote through a dangling symlink: %v", err)
						}
					} else if contents, err := os.ReadFile(source); err != nil || string(contents) != body {
						t.Errorf("symlink destination changed: %q, %v", contents, err)
					}
				})
			}
		})
	}
}

func expectMarkdownOwnershipConflict(t *testing.T, output string, result *Result, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Problems) != 1 || !strings.Contains(result.Problems[0].Error, output) {
		t.Errorf("expected one ownership problem for %s, got %+v", output, result.Problems)
	}
}

func TestSkillRouterLeavesMatchingCommandUnowned(t *testing.T) {
	home := sandbox(t)
	source := filepath.Join(home, "source")
	skill(t, filepath.Join(source, "keeper"), "keeper", "Keep this.")
	ok(t)(InstallSkills(source, []string{"keeper"}, []string{"claude"}))
	output := filepath.Join(home, ".claude", "commands", "keeper.md")
	ok(t)(SetSkillRouter(true))
	body := read(t, output)
	ok(t)(SetSkillRouter(false))
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("Library-created command survived disabling the router: %v", err)
	}

	write(t, output, body)
	ok(t)(SetSkillRouter(true))
	ok(t)(Sync())
	if applied := loaded(t).Applied["claude"]; applied != nil && len(applied.RouterCommands) != 0 {
		t.Error("the router claimed the user's matching command")
	}
	ok(t)(SetSkillRouter(false))
	if got := read(t, output); got != body {
		t.Errorf("the user's matching command changed: %q", got)
	}
}
