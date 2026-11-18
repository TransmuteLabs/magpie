package extension

import (
	"errors"
	"slices"
)

func ValidateAgentProcesses(manifest Manifest) error {
	if len(manifest.Processes) == 0 {
		if manifest.Contract.Has(AgentProcess) {
			return errors.New("agent-process requires a declared foreground action recipe")
		}
		return nil
	}
	if manifest.Contract.Version.Minor < 10 || !manifest.Contract.Has(AgentProcess) ||
		!manifest.Contract.Has(AgentWiring) || !manifest.Contract.Has(ModuleRPC) ||
		manifest.Component != nil || len(manifest.Processes) > 16 {
		return errors.New("owned processes require SDK API 1.10 and root module actions and agent wiring")
	}
	seen := map[string]bool{}
	for _, recipe := range manifest.Processes {
		if err := ValidateAgentProcessRecipe(recipe); err != nil {
			return err
		}
		if seen[recipe.ID] {
			return errors.New("duplicate process action recipe")
		}
		seen[recipe.ID] = true
		action, found := manifest.ResolveAction(recipe.ID)
		if !found || action.Effect != "write" || actionOwner(action) != PrimaryComponent {
			return errors.New("process recipe must select its own primary write action")
		}
		for _, provider := range manifest.Providers {
			if provider.Checkin != nil && provider.Checkin.Action == recipe.ID ||
				slices.ContainsFunc(provider.AccountTasks, func(task AccountTaskDescriptor) bool { return task.Action == recipe.ID }) {
				return errors.New("foreground process action cannot also be an account maintenance action")
			}
		}
		if !slices.ContainsFunc(manifest.Agents, func(agent AgentDescriptor) bool {
			return agent.ID == recipe.Agent && agent.Launch != nil
		}) {
			return errors.New("process recipe requires a launch-capable agent from its own package")
		}
	}
	return nil
}
