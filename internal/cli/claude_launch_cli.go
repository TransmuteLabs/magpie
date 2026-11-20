package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/claudecode"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/update"
)

// `magpie claude run [args…]` is claude-magpie (baranwang on X): Claude
// Code through magpie, while a plain `claude` stays on the user's own
// account. claude starts with --settings and magpie's launch file
// (agent/claude_launch.go), so ~/.claude/settings.json isn't switched;
// every argument is claude's, --help too. `magpie claude launcher` puts
// the claude-magpie command beside the magpie one on the PATH.

const claudeRunUsage = `magpie claude run [args…]      start Claude Code through magpie
magpie claude launcher [add|remove]
                               the claude-magpie command, which runs this

An SDK profile that owns Claude Code supplies its launch settings.
With the built-in owner, claude starts with --settings and a file wired
to magpie's gateway, key and models; your own ~/.claude/settings.json and a
plain claude stay as they are. Every argument after run is claude's.
`

// claudeLauncherName is the command a terminal runs for claude-magpie.
const claudeLauncherName = "claude-magpie"

// claudeLaunchCmd is `magpie claude run|launcher …`; ok false for
// another form.
func claudeLaunchCmd(args []string) (ok bool, err error) {
	if len(args) < 2 || args[0] != "claude" && args[0] != "claude-code" && args[0] != "cc" {
		return false, nil
	}
	switch args[1] {
	case "run":
		return true, claudeRun(args[2:])
	case "launcher":
		rest := args[2:]
		if len(rest) > 0 && slices.Contains([]string{"-h", "--help", "help"}, rest[0]) {
			fmt.Print(claudeRunUsage)
			return true, nil
		}
		return true, claudeLauncher(rest)
	}
	return false, nil
}

// claudeRunExec runs claude (claude_run_*.go) and waits for it; tests set it.
var claudeRunExec = claudeRunOS

// claudeFind is the Claude Code to start; tests set it.
var claudeFind = claudecode.Find

// claudeGatewayUp says whether magpie's gateway answers; tests set it.
var claudeGatewayUp = gateway.Running

// claudeLetGo hands claude the terminal (its Ctrl+C, its exit) and ends
// what magpie started (probes, plugin hosts), so nothing of magpie's runs
// beside a session for its hours; tests set it, since a host ended stays
// ended in the process.
var claudeLetGo = func() {
	leaveSignals()
	proc.EndProbes()
	plugin.EndHosts()
}

func claudeRun(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prepared, err := agent.PrepareExtensionProfileLaunch(ctx, "claude")
	if err != nil {
		return err
	}
	if prepared != nil {
		shutdown, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err := extension.Default().Close(shutdown)
		stop()
		if err != nil {
			return fmt.Errorf("stop SDK helpers before Claude launch: %w", err)
		}
		return startRunChecked(prepared.Starting, args, func() error {
			claudeLetGo()
			return prepared.Recheck(ctx)
		})
	}
	cancel()
	return claudeRunBuiltin(args)
}

func claudeRunBuiltin(args []string) error {
	bin := claudeFind()
	if bin == "" {
		return errors.New("Claude Code isn't installed: install it from https://claude.com/claude-code, " + claudecode.InstallHint)
	}
	if self, err := os.Executable(); err == nil && sameProgram(self, bin) {
		return errors.New("the claude on your PATH is magpie itself; install Claude Code")
	}
	home, _ := os.UserHomeDir()
	plugin.ServePages(false)
	startUp()
	var launchArgs []string
	var mark agent.ClaudeModelMark
	if err := agent.WithUnownedLocalAgent(home, "claude", func() error {
		path, err := agent.ClaudeLaunch(home)
		if err != nil {
			return fmt.Errorf("writing claude-magpie's settings: %w", err)
		}
		launchArgs, err = agent.ClaudeLaunchArgs(path)
		if err != nil {
			return fmt.Errorf("preparing claude-magpie's launch: %w", err)
		}
		mark = agent.ClaudeLaunchBefore(home)
		return nil
	}); err != nil {
		return err
	}
	if !claudeGatewayUp() {
		fmt.Fprintln(os.Stderr, "magpie: the gateway isn't running, so Claude Code's requests will fail: open magpie, or run `magpie serve`")
	}
	claudeLetGo()
	if err := agent.WithUnownedLocalAgent(home, "claude", func() error { return nil }); err != nil {
		return err
	}
	runErr := claudeRunExec(bin, append(launchArgs, args...), os.Environ())
	if err := agent.WithUnownedLocalAgent(home, "claude", func() error { return agent.ClaudeLaunchAfter(home, mark) }); err != nil {
		fmt.Fprintln(os.Stderr, "magpie: putting your settings.json's model back:", err)
	}
	return runErr
}

func claudeLauncher(args []string) error {
	what := "status"
	if len(args) > 0 {
		what = args[0]
	}
	if len(args) > 1 {
		return fmt.Errorf("magpie claude launcher takes one word, not %q", args[1])
	}
	switch what {
	case "status":
		if p := update.Launcher(claudeLauncherName); p != "" {
			fmt.Println(p)
			return nil
		}
		fmt.Println("claude-magpie isn't on your PATH: run `magpie claude launcher add`")
		return nil
	case "add":
		p, err := update.AddLauncher(claudeLauncherName, "claude", "run")
		if err != nil {
			return err
		}
		fmt.Println("claude-magpie:", p)
		return nil
	case "remove":
		p, err := update.RemoveLauncher(claudeLauncherName)
		if err != nil {
			return err
		}
		if p != "" {
			fmt.Println("removed", p)
		}
		return nil
	}
	return fmt.Errorf("magpie claude launcher: add, remove or nothing, not %q", what)
}
