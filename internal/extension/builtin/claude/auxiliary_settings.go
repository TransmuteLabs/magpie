package claude

import (
	"errors"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	AutoModeEnv = "CLAUDE_CODE_AUTO_MODE_MODEL"
	AdvisorEnv  = "CLAUDE_CODE_ENABLE_EXPERIMENTAL_ADVISOR_TOOL"
)

type AuxiliaryRole uint8

const (
	AutoModeClassifier AuxiliaryRole = iota
	Advisor
)

type AuxiliaryModelOptions struct {
	Role         AuxiliaryRole
	Model        string
	Previous     string
	GatewayRoute func(baseURL, token string) bool
}

func PlanAuxiliaryModel(options AuxiliaryModelOptions, settings sdk.ActionSettings) (sdk.AgentPlan, string, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != "settings.json" {
		return sdk.AgentPlan{}, "", errors.New("Claude auxiliary model action requires a settings.json snapshot")
	}
	file := settings.Files[0]
	current := gjson.ParseBytes(jsonc.ToJSON(edit.TrimBOM(file.Body)))
	if options.GatewayRoute == nil || !options.GatewayRoute(current.Get(routeURL).String(), current.Get(routeToken).String()) {
		return sdk.AgentPlan{}, "", errors.New("Claude Code is no longer routed through magpie")
	}
	var set []edit.KV
	var remove []string
	var next string
	switch options.Role {
	case AutoModeClassifier:
		const path = "env." + AutoModeEnv
		next = options.Model
		if next == "" {
			remove = append(remove, path)
		} else {
			set = append(set, edit.KV{Path: path, Value: next})
		}
	case Advisor:
		const path = "env." + AdvisorEnv
		const model = "advisorModel"
		switchValue := current.Get(path).String()
		owned := switchValue != "" && switchValue == options.Previous
		if options.Model == "" {
			remove = append(remove, model)
			if owned {
				remove = append(remove, path)
			}
		} else {
			next = options.Previous
			set = append(set, edit.KV{Path: model, Value: options.Model})
			if switchValue == "" || owned {
				next = "1"
				set = append(set, edit.KV{Path: path, Value: next})
			}
		}
	default:
		return sdk.AgentPlan{}, "", errors.New("unknown Claude auxiliary model role")
	}
	body, err := edit.PatchJSON(file.Body, set, remove)
	if err != nil {
		return sdk.AgentPlan{}, "", edit.AtFile(file.Path, err)
	}
	return routeSettingsPlan(file, body), next, nil
}

type StrayModelOptions struct {
	ModelEnv    []string
	MagpieModel func(string) bool
}

func PlanStrayModels(options StrayModelOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != "settings.json" || options.MagpieModel == nil {
		return sdk.AgentPlan{}, errors.New("Claude stray model cleanup requires settings.json and a model policy")
	}
	file := settings.Files[0]
	current := gjson.ParseBytes(jsonc.ToJSON(edit.TrimBOM(file.Body)))
	if current.Get(routeURL).String() != "" {
		return sdk.AgentPlan{}, nil
	}
	var remove []string
	for _, key := range options.ModelEnv {
		path := "env." + key
		if options.MagpieModel(current.Get(path).String()) {
			remove = append(remove, path)
		}
	}
	if options.MagpieModel(current.Get("model").String()) {
		remove = append(remove, "model")
	}
	if len(remove) == 0 {
		return sdk.AgentPlan{}, nil
	}
	body, err := edit.PatchJSON(file.Body, nil, remove)
	if err != nil {
		return sdk.AgentPlan{}, edit.AtFile(file.Path, err)
	}
	return routeSettingsPlan(file, body), nil
}
