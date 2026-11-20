package cline

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const ProvidersFile = "providers.json"

const EmptyProviders = `{"version":1,"modes":{},"providers":{}}`

func PlanEffort(effort string, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != ProvidersFile {
		return sdk.AgentPlan{}, errors.New("Cline effort requires the provider settings file")
	}
	file := settings.Files[0]
	raw := edit.TrimBOM(file.Body)
	if effort == "" && len(bytes.TrimSpace(raw)) == 0 {
		return sdk.AgentPlan{}, nil
	}
	provider := readSettings(file).Get("lastUsedProvider").String()
	if provider == "" {
		provider = "cline"
	}
	key := "providers." + provider + ".settings.reasoning"
	var set []edit.KV
	var remove []string
	body := file.Body
	if effort == "" {
		remove = []string{key}
	} else {
		if !file.Exists {
			body = []byte(EmptyProviders + "\n")
		}
		set = []edit.KV{{Path: key, Value: reasoning(effort)}}
	}
	body, err := edit.PatchJSON(body, set, remove)
	if err != nil {
		return sdk.AgentPlan{}, edit.AtFile(file.Path, err)
	}
	if bytes.Equal(body, file.Body) {
		return sdk.AgentPlan{}, nil
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: file.Path, BeforeHash: file.BeforeHash, Body: body}}}, nil
}

func reasoning(effort string) map[string]any {
	if effort == "none" {
		return map[string]any{"enabled": false}
	}
	return map[string]any{"enabled": true, "effort": effort}
}

func Effort(raw string) string {
	var reasoning struct {
		Enabled *bool  `json:"enabled"`
		Effort  string `json:"effort"`
	}
	if json.Unmarshal([]byte(raw), &reasoning) != nil {
		return ""
	}
	if reasoning.Enabled != nil && !*reasoning.Enabled {
		return "none"
	}
	return reasoning.Effort
}
