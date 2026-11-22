package agent

// Codex's status line, magpie's (lyj1115 on X: the current account's quota,
// or the relay's balance, under Codex's chat box, off unless turned on).
// Codex's TUI, from 0.171.0-alpha.12, draws its footer from a status line
// provider: a stdio MCP server whose tool advertises a statusline
// entrypoint, picked in config.toml's [tui] status_line_provider = {
// server, tool } behind [features] tui_extensions_statusline (its bundled
// create-statusline skill). `magpie statusline --codex` is such a server
// (internal/cli/statusline_codex.go). The switch on Codex's row writes
// those three keys, the server as this magpie's binary by its full path;
// off takes out only what is magpie's.
//
// A provider of the user's own, or an [mcp_servers.quota_line] of theirs,
// is never written over: the switch isn't offered while one is there, and
// on is refused; nor is a features flag they turned off. The flag is
// taken out on off only when magpie wrote it (stashed). Only offered
// where a Codex CLI here can draw it: 0.170.0 reads the keys without
// complaint and draws nothing. The desktop app has no such footer, and is
// not reached into (cockpit-tools drives it through its debug port).
// Only on this computer, as Claude Code's switch.

import (
	"fmt"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtincodex "github.com/yetone/magpie/internal/extension/builtin/codex"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	// codexStatusFrom is the first Codex that draws a provider's footer
	codexStatusFrom = "0.171.0-alpha.12"
	// codexStatusFlagKey stashes that magpie wrote the flag
	codexStatusFlagKey = "codex.statusline_feature"
)

func codexStatusRead(path string) builtincodex.StatusLineState {
	raw, err := edit.Read(path)
	if err != nil {
		return builtincodex.StatusLineState{}
	}
	return builtincodex.ReadStatusLine(raw)
}

// codexStatusDraws says whether a Codex CLI here draws a provider's
// footer.
func codexStatusDraws(at place) bool {
	for _, r := range codexReaders(at) {
		if r.Kind == "cli" && r.Version != "" && !Newer(codexStatusFrom, r.Version) {
			return true
		}
	}
	return false
}

// codexStatusField is the statusline switch of Codex's config.toml at
// path; here says the Codex is this computer's (not a distro's).
func codexStatusField(at place, path string, here bool) Field {
	flagKey := at.key(codexStatusFlagKey)
	return Field{
		Key: "statusline", Label: "status line", Quiet: true, Switch: true,
		Get: func() string {
			if codexStatusRead(path).Owned() {
				return "on"
			}
			return ""
		},
		Set: func(value string) error {
			switch value {
			case "", "off", "on":
			default:
				return fmt.Errorf("status line is on or off, not %q", value)
			}
			before := peek(flagKey)
			options := builtincodex.StatusLineOptions{
				Enable: value == "on", Local: here, OwnedFlag: before[flagKey] != "",
				NativePath: at.native(path), InstallCommand: claudeStatusExe,
			}
			var ownedFlag bool
			err := extension.ApplyLinkedAgentPlan(options, func(options builtincodex.StatusLineOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
				plan, after, err := builtincodex.PlanStatusLine(options, settings)
				ownedFlag = after
				return plan, err
			}, extension.LinkedActionFile{Name: builtincodex.GlobalConfigFile, Path: path})
			if err != nil {
				return err
			}
			if ownedFlag && !options.OwnedFlag {
				stash(map[string]string{flagKey: "1"})
			} else if !ownedFlag {
				forgetValues(before)
			}
			return nil
		},
		Options: func(map[string]string) []Option {
			if !here {
				return nil
			}
			state := codexStatusRead(path)
			if !state.Owned() && (!state.Free() || !codexStatusDraws(at)) {
				return nil
			}
			return []Option{{Value: "on", Note: "the remaining quota or balance of the account in use, under Codex's prompt"}}
		},
	}
}
