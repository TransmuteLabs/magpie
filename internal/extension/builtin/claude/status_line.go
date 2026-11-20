package claude

import (
	"errors"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var ErrStatusLineOccupied = errors.New("Claude Code has a status line of its own")

type StatusLineOptions struct {
	Enable         bool
	InstallCommand func() (string, error)
}

func PlanStatusLine(options StatusLineOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != "settings.json" {
		return sdk.AgentPlan{}, errors.New("Claude status line action requires a settings.json snapshot")
	}
	file := settings.Files[0]
	current := gjson.GetBytes(jsonc.ToJSON(edit.TrimBOM(file.Body)), "statusLine")
	owned := StatusLineOurs(current.Get("command").String())
	if options.Enable == owned {
		return sdk.AgentPlan{}, nil
	}
	var set []edit.KV
	var remove []string
	if options.Enable {
		if current.Exists() {
			return sdk.AgentPlan{}, ErrStatusLineOccupied
		}
		if options.InstallCommand == nil {
			return sdk.AgentPlan{}, errors.New("Claude status line installation requires a command")
		}
		command, err := options.InstallCommand()
		if err != nil {
			return sdk.AgentPlan{}, err
		}
		set = []edit.KV{{Path: "statusLine", Value: map[string]any{"type": "command", "command": command}}}
	} else {
		remove = []string{"statusLine"}
	}
	body, err := edit.PatchJSON(file.Body, set, remove)
	if err != nil {
		return sdk.AgentPlan{}, edit.AtFile(file.Path, err)
	}
	return routeSettingsPlan(file, body), nil
}

func StatusLineOurs(command string) bool {
	program, rest, ok := statusLineWord(strings.TrimLeft(command, " \t"))
	if !ok || program == "" {
		return false
	}
	base := strings.ToLower(program[strings.LastIndexAny(program, `/\`)+1:])
	base = strings.TrimSuffix(base, ".exe")
	if base != "magpie" && !strings.HasPrefix(base, "magpie-") {
		return false
	}
	for index := 0; ; index++ {
		rest = strings.TrimLeft(rest, " \t")
		if rest == "" {
			return index > 0
		}
		var argument string
		if argument, rest, ok = statusLineWord(rest); !ok || index == 0 && argument != "statusline" {
			return false
		}
	}
}

func statusLineWord(command string) (word, rest string, ok bool) {
	var output strings.Builder
	for offset := 0; offset < len(command); offset++ {
		switch character := command[offset]; character {
		case ' ', '\t':
			return output.String(), command[offset:], true
		case '\n', ';', '&', '|', '<', '>', '(', ')', '`', '$':
			return "", "", false
		case '\'':
			end := strings.IndexByte(command[offset+1:], '\'')
			if end < 0 {
				return "", "", false
			}
			output.WriteString(command[offset+1 : offset+1+end])
			offset += 1 + end
		case '"':
			for offset++; ; offset++ {
				if offset >= len(command) {
					return "", "", false
				}
				if command[offset] == '"' {
					break
				}
				if command[offset] == '$' || command[offset] == '`' {
					return "", "", false
				}
				if command[offset] == '\\' && offset+1 < len(command) && strings.IndexByte("\"\\$`", command[offset+1]) >= 0 {
					offset++
				}
				output.WriteByte(command[offset])
			}
		case '\\':
			if offset+1 < len(command) {
				offset++
				output.WriteByte(command[offset])
			}
		default:
			output.WriteByte(character)
		}
	}
	return output.String(), "", true
}
