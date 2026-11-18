package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/agent"
)

func TestAgentHelpDoesNotPrepareLaunch(t *testing.T) {
	root := helpHome(t)
	marker := filepath.Join(root, "launch-was-prepared")
	starts := 0
	a := &agent.Agent{
		ID: "fixture", Name: "Fixture", Bin: "fixture-command",
		Fields: []agent.Field{{Key: "model", Label: "Model"}},
		Start: func() agent.Starting {
			starts++
			if err := os.WriteFile(marker, []byte("prepared"), 0o600); err != nil {
				t.Fatal(err)
			}
			return agent.Starting{Bin: "fixture-command"}
		},
	}
	before := tree(t, root)
	out := agentHelp(a)
	if starts != 0 {
		t.Fatalf("help prepared an agent launch %d times", starts)
	}
	if !strings.Contains(out, "start fixture-command,") {
		t.Fatalf("help lost the declared launch command: %q", out)
	}
	if after := tree(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("help wrote under the home: before %v, after %v", before, after)
	}
}

func TestAgentHelpDoesNotCreateSDKOwnershipLocks(t *testing.T) {
	root := helpHome(t)
	before := tree(t, root)
	for _, args := range [][]string{
		{"agy", "--help"}, {"agy", "-h"}, {"agy", "help"}, {"agy", "model", "--help"}, {"agy", "--help", "model"},
	} {
		out, err := runOut(t, args...)
		if err != nil || !strings.Contains(out, "magpie agy run") {
			t.Fatalf("magpie %s: %v, %q", strings.Join(args, " "), err, out)
		}
		after := tree(t, root)
		for name := range after {
			if filepath.Base(name) == "sdk-agent-locks" {
				t.Fatalf("help created SDK ownership locks: %s", name)
			}
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("help wrote under the home: before %v, after %v", before, after)
		}
	}
}

func TestAgentHelpPreservesLegacyLaunchTextWithoutStarting(t *testing.T) {
	root := helpHome(t)
	managedCodex := filepath.Join(root, "managed-codex")
	t.Setenv("CODEX_HOME", managedCodex)
	distroRoot := t.TempDir()
	t.Cleanup(agent.FakeWSL(map[string]string{
		"HelpDistro": "home:/home/me\ndir:.claude\ndir:.codex\ndir:.gemini/antigravity-cli\ndir:.config/opencode\n",
	}, map[string]string{"HelpDistro": distroRoot}))
	seen := map[string]bool{}
	wslLaunches := 0
	all := agent.All()
	for _, a := range all {
		seen[a.ID] = true
		t.Run(a.ID, func(t *testing.T) {
			if a.ID == "codex" && filepath.Dir(a.Path) != managedCodex {
				t.Fatal("help comparison did not use the managed Codex home", a.Path)
			}
			expected := *a
			if a.Start != nil {
				prior := a.Start()
				expected.Bin = prior.Bin
				expected.Start = func() agent.Starting { return prior }
				if a.WSL != "" {
					wslLaunches++
				} else if a.Bin != prior.Bin {
					t.Fatalf("declared command %q differs from legacy start command %q", a.Bin, prior.Bin)
				}
			}
			want := agentHelp(&expected)
			starts := 0
			if a.Start != nil {
				a.Start = func() agent.Starting {
					starts++
					return agent.Starting{Bin: "unexpected-launch-preparation"}
				}
			}
			before := tree(t, root)
			if got := agentHelp(a, all); got != want {
				t.Fatalf("help changed its legacy text:\nwant %s\ngot %s", want, got)
			}
			if a.WSL != "" {
				if got := agentHelp(a, nil); got != want {
					t.Fatalf("help lost its declared WSL command without a local counterpart:\nwant %s\ngot %s", want, got)
				}
			}
			if starts != 0 {
				t.Fatalf("help prepared a launch %d times", starts)
			}
			if after := tree(t, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("help wrote under the home: before %v, after %v", before, after)
			}
		})
	}
	if !seen["codex"] || !seen["claude-desktop"] || wslLaunches == 0 {
		t.Fatal("legacy help comparison missed desktop, managed Codex or WSL launch metadata", seen, wslLaunches)
	}
}
