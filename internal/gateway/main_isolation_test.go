package gateway

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
)

func TestGatewayTestMainKeepsClaudeDiscoveryIsolated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the synthetic Claude installs are shell scripts")
	}
	const childEnv = "MAGPIE_TEST_GATEWAY_CLAUDE_ISOLATION"
	if hostDir := os.Getenv(childEnv); hostDir != "" {
		fixtureDir := t.TempDir()
		fixture := filepath.Join(fixtureDir, "claude")
		testenv.Program(t, fixture, "#!/bin/sh\nprintf '2.1.38 (Claude Code)\\n'\n")
		t.Setenv("PATH", fixtureDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		cli, err := claudeBinary()
		if err != nil || cli.path != fixture {
			t.Fatalf("Claude discovery selected %q (%v), want test fixture %q", cli.path, err, fixture)
		}
		for _, name := range []string{"node", "bun"} {
			utility := filepath.Join(hostDir, name)
			if found, err := exec.LookPath(name); err != nil || found != utility {
				t.Fatalf("%s unavailable after isolation: got %q (%v), want %q", name, found, err, utility)
			}
			out, err := proc.CommandContext(testenv.DeadlineContext(t, time.Second), utility).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != name {
				t.Fatalf("%s did not run after isolation: %q (%v)", name, out, err)
			}
		}
		return
	}

	hostDir := t.TempDir()
	versions := filepath.Join(hostDir, "versions")
	if err := os.Mkdir(versions, 0o755); err != nil {
		t.Fatal(err)
	}
	hostClaude := filepath.Join(versions, "99.0.0")
	if err := os.WriteFile(hostClaude, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hostClaude, filepath.Join(hostDir, "claude")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"node", "bun"} {
		testenv.Program(t, filepath.Join(hostDir, name), "#!/bin/sh\nprintf '"+name+"\\n'\n")
	}
	cmd := proc.CommandContext(testenv.DeadlineContext(t, time.Second), os.Args[0], "-test.run=^TestGatewayTestMainKeepsClaudeDiscoveryIsolated$")
	cmd.Env = append(os.Environ(), childEnv+"="+hostDir, standInsEnv+"=", "PATH="+hostDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gateway test binary did not isolate discovery: %v\n%s", err, out)
	}
}
