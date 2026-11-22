package agent

// Claude Code's status line, magpie's (steven_4422 on Discord: "show 5h
// and 7d account usage in claude code"). `magpie statusline` (#1997)
// prints the session's model and each account's windows as the gateway
// last read them, but only a user who found it in the reference and wrote
// it into settings.json had it. The Agents page offers it as a switch on
// Claude Code's row: on writes settings.json's statusLine to run this
// magpie's binary by its full path (a terminal's `magpie` may be another
// copy, or none: the app from the site puts none on PATH), off takes out
// only a status line that runs `magpie statusline`. A status line of the
// user's own (claude-hud, a script) is never written over: the switch
// isn't offered while one is there, and on is refused.
//
// Claude Code (2.1.316) watches settings.json and runs a changed
// statusLine command in its open sessions too, with the session's JSON on
// stdin, after each change of its messages; so nothing needs a restart.
// Only on this computer: a WSL distro's Claude Code would run a Windows
// binary through interop, which isn't tried.

import (
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtinclaude "github.com/yetone/magpie/internal/extension/builtin/claude"
	"github.com/yetone/magpie/internal/update"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// claudeStatusExe is the magpie a status line written now runs; a var for
// tests.
var claudeStatusExe = func() string {
	if exe, err := update.Program(); err == nil && exe != "" {
		return exe
	}
	return "magpie"
}

// claudeStatusCommand is the statusLine command that runs exe's
// statusline: by a path Claude Code's shell takes as one word (it runs a
// status line through bash, Git Bash on Windows, where a path is written
// with forward slashes).
func claudeStatusCommand(exe, goos string) string {
	if goos == "windows" {
		exe = strings.ReplaceAll(exe, `\`, "/")
	}
	if strings.ContainsAny(exe, " \t\n'\"\\$`!&;|<>()*?[]#~{}") {
		exe = "'" + strings.ReplaceAll(exe, "'", `'\''`) + "'"
	}
	return exe + " statusline"
}

// claudeStatusField is the statusline switch of Claude Code's settings.json
// at path; settled is called after each write (no open session needs a
// restart for it).
func claudeStatusField(at place, path string, settled func()) Field {
	// mine: settings.json has a status line; ours: it runs magpie's
	cur := func() (has, ours bool) {
		status, has := edit.GetJSONRaw(path, "statusLine")
		return has, has && builtinclaude.StatusLineOurs(gjson.Get(status, "command").String())
	}
	here := at.base == nil
	return Field{
		Key: "statusline", Label: "status line", Quiet: true, Switch: true,
		Get: func() string {
			if _, ours := cur(); ours {
				return "on"
			}
			return ""
		},
		Set: func(v string) error {
			if v != "" && v != "off" && v != "on" {
				return fmt.Errorf("status line is on or off, not %q", v)
			}
			options := builtinclaude.StatusLineOptions{
				Enable: v == "on",
				InstallCommand: func() (string, error) {
					if !here {
						return "", errors.New("magpie's status line is for Claude Code on this computer, not in WSL")
					}
					return claudeStatusCommand(claudeStatusExe(), runtime.GOOS), nil
				},
			}
			var changed bool
			err := extension.ApplyLinkedAgentPlan(options, func(options builtinclaude.StatusLineOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
				plan, err := builtinclaude.PlanStatusLine(options, settings)
				if errors.Is(err, builtinclaude.ErrStatusLineOccupied) {
					return sdk.AgentPlan{}, fmt.Errorf("%s has a status line of its own; magpie leaves it as it is (take it out there first, or add `magpie statusline` to the command it runs)", at.native(path))
				}
				changed = len(plan.Files) != 0
				return plan, err
			}, extension.LinkedActionFile{Name: "settings.json", Path: path})
			if err == nil && changed {
				settled()
			}
			return err
		},
		Options: func(map[string]string) []Option {
			// a distro's file isn't read: a stopped one would start (#3896)
			if !here {
				return nil
			}
			if has, ours := cur(); has && !ours {
				return nil
			}
			return []Option{{Value: "on", Note: "the 5h and 7d use of the accounts magpie serves, under Claude Code's prompt"}}
		},
	}
}
