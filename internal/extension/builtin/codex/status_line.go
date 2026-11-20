package codex

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	statusLineServer   = "quota_line"
	statusLineTool     = "statusline"
	statusLineFeature  = "tui_extensions_statusline"
	statusLineProvider = "status_line_provider"
	statusLineTable    = "mcp_servers." + statusLineServer
)

type StatusLineState struct {
	readable, mine, server, serverOurs, picked bool
	flag                                       string
}

func (state StatusLineState) Owned() bool { return state.picked && state.serverOurs }

func (state StatusLineState) Free() bool {
	return state.readable && !state.mine && (!state.server || state.serverOurs) && state.flag != "false"
}

func ReadStatusLine(raw []byte) StatusLineState {
	var doc struct {
		Features   map[string]any `toml:"features"`
		McpServers map[string]any `toml:"mcp_servers"`
		Tui        struct {
			Provider any `toml:"status_line_provider"`
		} `toml:"tui"`
	}
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return StatusLineState{}
	}
	state := StatusLineState{readable: true}
	switch flag := doc.Features[statusLineFeature].(type) {
	case bool:
		state.flag = fmt.Sprint(flag)
	case nil:
	default:
		state.flag = "false"
	}
	if server, ok := doc.McpServers[statusLineServer]; ok {
		state.server = true
		config, _ := server.(map[string]any)
		command, _ := config["command"].(string)
		args, _ := config["args"].([]any)
		base := strings.ToLower(filepath.Base(strings.ReplaceAll(command, `\`, "/")))
		base = strings.TrimSuffix(base, ".exe")
		state.serverOurs = (base == "magpie" || strings.HasPrefix(base, "magpie-")) &&
			len(args) == 2 && args[0] == "statusline" && args[1] == "--codex"
	}
	switch provider := doc.Tui.Provider.(type) {
	case nil:
	case string:
		state.mine = provider != "default"
	case map[string]any:
		state.picked = provider["server"] == statusLineServer && provider["tool"] == statusLineTool
		state.mine = !state.picked
	default:
		state.mine = true
	}
	return state
}

type StatusLineOptions struct {
	Enable         bool
	Local          bool
	OwnedFlag      bool
	NativePath     string
	InstallCommand func() string
}

func PlanStatusLine(options StatusLineOptions, settings sdk.ActionSettings) (sdk.AgentPlan, bool, error) {
	var plan sdk.AgentPlan
	if len(settings.Files) != 1 || settings.Files[0].Path != GlobalConfigFile {
		return plan, options.OwnedFlag, errors.New("Codex status line action requires a config.toml snapshot")
	}
	config := settings.Files[0]
	raw := edit.TrimBOM(config.Body)
	state := ReadStatusLine(raw)
	body := raw
	ownsFlag := options.OwnedFlag
	var err error
	if options.Enable {
		switch {
		case state.Owned():
			return plan, ownsFlag, nil
		case !options.Local:
			return plan, ownsFlag, errors.New("magpie's status line is for Codex on this computer, not in WSL")
		case !state.readable:
			return plan, ownsFlag, fmt.Errorf("%s can't be read; magpie leaves it as it is", options.NativePath)
		case !state.Free():
			return plan, ownsFlag, fmt.Errorf("%s has a status line provider, an [mcp_servers.%s] or a %s of its own; magpie leaves them as they are", options.NativePath, statusLineServer, statusLineFeature)
		case options.InstallCommand == nil:
			return plan, ownsFlag, errors.New("Codex status line installation requires a command")
		}
		addFlag := state.flag == ""
		body, err = installStatusLine(config.Path, raw, options.InstallCommand(), addFlag)
		if err == nil && !ReadStatusLine(body).Owned() {
			err = fmt.Errorf("%s didn't read back as magpie wrote it; it is left as it was", options.NativePath)
		}
		ownsFlag = ownsFlag || addFlag
	} else if state.readable {
		removeFlag := ownsFlag && state.flag == "true"
		body, err = removeStatusLine(config.Path, raw, state, removeFlag)
		if err == nil && (state.picked || state.serverOurs || removeFlag) {
			after := state
			if !bytes.Equal(body, raw) {
				after = ReadStatusLine(body)
			}
			if !after.readable || after.picked && state.picked || after.serverOurs && state.serverOurs || removeFlag && after.flag == "true" {
				err = fmt.Errorf("could not remove magpie's status line from %s", options.NativePath)
			}
		}
		ownsFlag = false
	}
	if err != nil {
		return plan, options.OwnedFlag, err
	}
	if !bytes.Equal(body, raw) {
		plan.Files = []sdk.FileChange{{Path: config.Path, Body: body, BeforeHash: config.BeforeHash}}
	}
	return plan, ownsFlag, nil
}

func installStatusLine(path string, raw []byte, command string, addFlag bool) ([]byte, error) {
	var err error
	if addFlag {
		raw, err = edit.RewriteTOMLKey(path, raw, "features", statusLineFeature, true)
		if err != nil {
			return nil, err
		}
	}
	raw, err = edit.RewriteTOMLTable(path, raw, statusLineTable, func(edit.TOMLTable) []edit.KV {
		return []edit.KV{
			{Path: "command", Value: command},
			{Path: "args", Value: edit.Raw("['statusline', '--codex']")},
			{Path: "env_vars", Value: edit.Raw("['MAGPIE_ADDR']")},
		}
	})
	if err != nil {
		return nil, err
	}
	return edit.RewriteTOMLKey(path, raw, "tui", statusLineProvider, edit.Inline{
		{Path: "server", Value: statusLineServer}, {Path: "tool", Value: statusLineTool},
	})
}

func removeStatusLine(path string, raw []byte, state StatusLineState, removeFlag bool) ([]byte, error) {
	var err error
	if state.picked {
		raw, err = edit.RewriteTOMLWithoutKey(path, raw, "tui", statusLineProvider)
		if err != nil {
			return nil, err
		}
	}
	if state.serverOurs {
		raw, err = edit.RewriteTOMLWithoutTable(path, raw, statusLineTable)
		if err != nil {
			return nil, err
		}
	}
	if removeFlag {
		return edit.RewriteTOMLWithoutKey(path, raw, "features", statusLineFeature)
	}
	return raw, nil
}
