package codex

import (
	"errors"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type EffortPlanOptions struct {
	Level   string
	Catalog []byte
}

func PlanEffort(options EffortPlanOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	var plan sdk.AgentPlan
	if options.Catalog != nil {
		var err error
		plan, err = PlanCatalog(options.Catalog, settings)
		if err != nil {
			return sdk.AgentPlan{}, err
		}
	} else if len(settings.Files) != 1 {
		return sdk.AgentPlan{}, errors.New("Codex effort action requires a config snapshot")
	}
	file := settings.Files[0]
	if file.Path != GlobalConfigFile && file.Path != ProfileConfigFile {
		return sdk.AgentPlan{}, errors.New("invalid Codex effort snapshot path")
	}
	if !file.Exists && options.Level == "" {
		return plan, nil
	}
	const key = "model_reasoning_effort"
	raw := edit.TrimBOM(file.Body)
	var body []byte
	var err error
	if options.Level == "" {
		body, err = edit.RewriteTOMLWithoutTop(file.Path, raw, key)
	} else {
		body, err = edit.RewriteTOMLTop(file.Path, raw, edit.KV{Path: key, Value: options.Level})
	}
	if err != nil {
		return sdk.AgentPlan{}, err
	}
	plan.Files = append(plan.Files, sdk.FileChange{Path: file.Path, Body: body, BeforeHash: file.BeforeHash})
	return plan, nil
}
