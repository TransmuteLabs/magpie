package extension

import (
	"errors"
	"slices"
	"strings"
)

type AgentDecisionModel struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Provider  string   `json:"provider,omitempty"`
	Group     bool     `json:"group,omitempty"`
	Endpoints []string `json:"endpoints"`
}

func (model AgentDecisionModel) Clone() AgentDecisionModel {
	model.Endpoints = slices.Clone(model.Endpoints)
	return model
}

func ValidateAgentDecisionModels(manifest Manifest, models []AgentDecisionModel) error {
	if manifest.Contract.Version.Major != 1 || manifest.Contract.Version.Minor < 9 || !manifest.Contract.Has(AgentWiring) {
		return errors.New("agent decision models require SDK API 1.9 agent-wiring")
	}
	if models == nil {
		return errors.New("agent decision models must be an array")
	}
	for _, model := range models {
		if strings.TrimSpace(model.ID) == "" || strings.TrimSpace(model.Name) == "" ||
			model.Group && model.Provider != "" || !model.Group && strings.TrimSpace(model.Provider) == "" {
			return errors.New("agent decision model requires an id, name and either provider or group")
		}
		if len(model.Endpoints) == 0 || len(model.Endpoints) > 2 {
			return errors.New("agent decision model requires decision endpoints")
		}
		seen := map[string]bool{}
		for _, endpoint := range model.Endpoints {
			if endpoint != "/v1/systemone" && endpoint != "/v1/decisions" || seen[endpoint] {
				return errors.New("invalid or duplicate agent decision endpoint")
			}
			seen[endpoint] = true
		}
	}
	return nil
}
