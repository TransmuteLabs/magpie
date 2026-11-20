package agent

import (
	"github.com/yetone/magpie/internal/extension"
	builtinclaude "github.com/yetone/magpie/internal/extension/builtin/claude"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	claudeAutoModeKey = "claude.auto_mode_model"
	claudeAdvisorKey  = "claude.advisor_env"
)

func applyClaudeAuxiliaryModel(at place, path string, role builtinclaude.AuxiliaryRole, model string) error {
	key := at.key(claudeAutoModeKey)
	if role == builtinclaude.Advisor {
		key = at.key(claudeAdvisorKey)
	}
	options := builtinclaude.AuxiliaryModelOptions{
		Role: role, Model: model, Previous: stashShared()[key],
		GatewayRoute: func(baseURL, token string) bool {
			gateway := at.gw()
			if baseURL == gateway {
				return true
			}
			_, proxied := proxyBase(baseURL, gateway)
			return proxied && claudeKeyedToken(at, token) && surelyMagpie(baseURL, gateway)
		},
	}
	var next string
	err := extension.ApplyLinkedAgentPlan(options, func(options builtinclaude.AuxiliaryModelOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
		plan, claim, err := builtinclaude.PlanAuxiliaryModel(options, settings)
		next = claim
		return plan, err
	}, extension.LinkedActionFile{Name: "settings.json", Path: path})
	if err == nil && next != options.Previous {
		stash(map[string]string{key: next})
	}
	return err
}

func dropClaudeStrayModels(path string) error {
	options := builtinclaude.StrayModelOptions{ModelEnv: claudeModelEnv, MagpieModel: claudeMagpieModel}
	return extension.ApplyLinkedAgentPlan(options, builtinclaude.PlanStrayModels,
		extension.LinkedActionFile{Name: "settings.json", Path: path})
}
