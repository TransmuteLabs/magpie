package claude

import (
	"bytes"
	"errors"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const EffortEnv = "CLAUDE_CODE_EFFORT_LEVEL"

type EffortPlanOptions struct {
	ModelKey string
	ReadsTop bool
	Level    string
}

func PlanEffort(options EffortPlanOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != "settings.json" {
		return sdk.AgentPlan{}, errors.New("Claude effort action requires a settings.json snapshot")
	}
	file := settings.Files[0]
	body, err := effortSettings(file.Body, options)
	if err != nil || bytes.Equal(file.Body, body) {
		return sdk.AgentPlan{}, edit.AtFile(file.Path, err)
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: file.Path, BeforeHash: file.BeforeHash, Body: body}}}, nil
}

func effortSettings(body []byte, options EffortPlanOptions) ([]byte, error) {
	envPath := "env." + EffortEnv
	if options.Level == "max" {
		return edit.PatchJSON(body, []edit.KV{{Path: envPath, Value: options.Level}}, nil)
	}
	plain := gjson.ParseBytes(jsonc.ToJSON(edit.TrimBOM(body)))
	var set []edit.KV
	var remove []string
	if plain.Get(envPath).String() != "" {
		remove = append(remove, envPath)
	}
	modelPath := "modelSettings." + options.ModelKey
	modelEffort := modelPath + ".effortLevel"
	if options.Level != "" {
		set = append(set, edit.KV{Path: "effortLevel", Value: options.Level})
		if options.ModelKey != "" {
			set = append(set, edit.KV{Path: modelEffort, Value: options.Level})
		}
		return edit.PatchJSON(body, set, remove)
	}
	if len(bytes.TrimSpace(edit.TrimBOM(body))) == 0 {
		return body, nil
	}
	top := plain.Get("effortLevel")
	own := plain.Get(modelEffort).String()
	if options.ModelKey != "" {
		remove = append(remove, modelEffort)
	}
	if options.ReadsTop || top.String() != "" && top.String() == own {
		remove = append(remove, "effortLevel")
	}
	if len(remove) == 0 {
		return body, nil
	}
	out, err := edit.PatchJSON(body, nil, remove)
	if err != nil || options.ModelKey == "" {
		return out, err
	}
	for _, path := range []string{modelPath, "modelSettings"} {
		object := gjson.GetBytes(jsonc.ToJSON(edit.TrimBOM(out)), path)
		if strings.Join(strings.Fields(object.Raw), "") == "{}" {
			out, err = edit.PatchJSON(out, nil, []string{path})
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
