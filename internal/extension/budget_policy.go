package extension

import (
	"context"
	"errors"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

var ErrBudgetPolicyUnavailable = errors.New("the selected gateway budget policy is unavailable, so nothing was sent")

type budgetPolicyBinding struct {
	entry         Installed
	descriptor    sdk.BudgetPolicyDescriptor
	configVersion string
}

func (m *Manager) budgetPolicy(ctx context.Context, selection string) (budgetPolicyBinding, error) {
	module, policy, valid := sdk.SplitBudgetPolicy(selection)
	if !valid {
		return budgetPolicyBinding{}, ErrBudgetPolicyUnavailable
	}
	entries, err := m.catalog.list(ctx)
	if err != nil {
		return budgetPolicyBinding{}, ErrBudgetPolicyUnavailable
	}
	for _, entry := range entries {
		if entry.Manifest.ID != module || entry.Disabled || entry.observerEnablementErr != nil {
			continue
		}
		for _, descriptor := range entry.Manifest.BudgetPolicies {
			if descriptor.ID != policy {
				continue
			}
			config, err := m.runtimeModuleConfigContext(ctx, entry.Manifest)
			if err != nil {
				return budgetPolicyBinding{}, ErrBudgetPolicyUnavailable
			}
			return budgetPolicyBinding{entry, descriptor, config.Version}, nil
		}
	}
	return budgetPolicyBinding{}, ErrBudgetPolicyUnavailable
}

func (m *Manager) EvaluateBudget(parent context.Context, selection, method string, request sdk.BudgetPolicyRequest) (sdk.BudgetPolicyResult, error) {
	binding, err := m.budgetPolicy(parent, selection)
	if err != nil {
		return sdk.BudgetPolicyResult{}, err
	}
	request.Policy = binding.descriptor.ID
	timeout := binding.descriptor.TimeoutMs
	if timeout == 0 {
		timeout = sdk.BudgetDefaultTimeoutMs
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	lease, err := m.acquireLease(ctx, binding.entry.Manifest.ID, &expectedGeneration{
		packageRevision: binding.entry.Revision, configVersion: binding.configVersion, enableGeneration: &binding.entry.EnableGeneration,
	})
	if err != nil {
		return sdk.BudgetPolicyResult{}, ErrBudgetPolicyUnavailable
	}
	defer lease.Close()
	raw, err := lease.Call(ctx, method, sdk.Scope{}, request, nil)
	if err != nil {
		return sdk.BudgetPolicyResult{}, ErrBudgetPolicyUnavailable
	}
	current, err := m.budgetPolicy(ctx, selection)
	if err != nil || ctx.Err() != nil || current.entry.Revision != binding.entry.Revision ||
		current.entry.EnableGeneration != binding.entry.EnableGeneration || current.configVersion != binding.configVersion {
		return sdk.BudgetPolicyResult{}, ErrBudgetPolicyUnavailable
	}
	return sdk.DecodeBudgetResult(method, request, raw)
}
