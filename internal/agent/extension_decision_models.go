package agent

import (
	"encoding/json"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func extensionAgentDecisionModels(agent string, manifest sdk.Manifest) ([]sdk.AgentDecisionModel, error) {
	raw, err := json.Marshal(pluggedDecisionModels(agent))
	if err != nil {
		return nil, err
	}
	var models []sdk.AgentDecisionModel
	if err := json.Unmarshal(raw, &models); err != nil {
		return nil, err
	}
	if err := sdk.ValidateAgentDecisionModels(manifest, models); err != nil {
		return nil, err
	}
	return models, nil
}
