package extension

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type AgentProcessResolver func(context.Context, *Lease, sdk.AgentProcessRecipe) (OwnedProcessLaunch, error)

var processAgentResolver struct {
	sync.RWMutex
	resolve AgentProcessResolver
}

func RegisterAgentProcessResolver(resolve AgentProcessResolver) {
	processAgentResolver.Lock()
	defer processAgentResolver.Unlock()
	if resolve == nil || processAgentResolver.resolve != nil {
		panic("agent process resolver must have one host owner")
	}
	processAgentResolver.resolve = resolve
}

func (lease *Lease) callDeclaredProcess(ctx context.Context, scope sdk.Scope, action sdk.ActionDescriptor,
	params any, consume func(Event) error,
) (bool, json.RawMessage, error) {
	manifest := lease.Manifest()
	position := slices.IndexFunc(manifest.Processes, func(recipe sdk.AgentProcessRecipe) bool {
		return recipe.ID == action.ID
	})
	if position < 0 {
		return false, nil, nil
	}
	refuse := func(message string) (bool, json.RawMessage, error) {
		return true, nil, &OutcomeError{Outcome: "not-started", Cause: errors.New(message)}
	}
	for _, provider := range manifest.Providers {
		if provider.Checkin != nil && provider.Checkin.Action == action.ID ||
			slices.ContainsFunc(provider.AccountTasks, func(task sdk.AccountTaskDescriptor) bool { return task.Action == action.ID }) {
			return refuse("background account actions cannot own foreground processes")
		}
	}
	readOnly, _ := ctx.Value(gatewayReadOnlyKey{}).(bool)
	if !manifest.Contract.Has(sdk.AgentProcess) || !manifest.Contract.Has(sdk.ModuleRPC) ||
		action.Effect != "write" || action.Component != "" && action.Component != sdk.PrimaryComponent ||
		scope.Operation == "" || scope.Attempt == 0 || readOnly ||
		ctx.Value(hostAuthorityKey{}) != nil || ctx.Value(componentAuthorityKey{}) != nil ||
		ctx.Value(ownCallParentKey{}) != nil || ctx.Value(accountTaskParentKey{}) != nil ||
		ctx.Value(accountTaskExecutionKey{}) != nil {
		return refuse("declared process requires its original foreground write action")
	}
	processAgentResolver.RLock()
	resolve := processAgentResolver.resolve
	processAgentResolver.RUnlock()
	if resolve == nil {
		return refuse("agent process resolution is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	recipe := manifest.Processes[position]
	recipe.Argv = slices.Clone(recipe.Argv)
	launch, err := resolve(ctx, lease, recipe)
	if err != nil {
		return true, nil, &OutcomeError{Outcome: "not-started", Cause: err}
	}
	if launch.Recipe.ID != recipe.ID || launch.Recipe.Agent != recipe.Agent {
		return refuse("host process resolution changed its installed recipe owner")
	}
	result, err := lease.CallWithProcess(ctx, launch, sdk.MethodModuleCall, scope, params, consume)
	return true, result, err
}
