package extension

import (
	"context"
	"errors"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

var ErrBudgetPolicyUnavailable = errors.New("the selected gateway budget policy is unavailable, so nothing was sent")

func (m *Manager) EvaluateBudget(parent context.Context, selection, method string, request sdk.BudgetPolicyRequest) (sdk.BudgetPolicyResult, error) {
	module, policy, valid := sdk.SplitBudgetPolicy(selection)
	if !valid {
		return sdk.BudgetPolicyResult{}, ErrBudgetPolicyUnavailable
	}
	declared := func(manifest sdk.Manifest) []sdk.BudgetPolicyDescriptor {
		for _, descriptor := range manifest.BudgetPolicies {
			if descriptor.ID == policy {
				return []sdk.BudgetPolicyDescriptor{descriptor}
			}
		}
		return nil
	}
	bindings := registeredContributions(parent, m, module, declared)
	if len(bindings) != 1 {
		return sdk.BudgetPolicyResult{}, ErrBudgetPolicyUnavailable
	}
	binding := bindings[0]
	request.Policy = binding.Descriptor.ID
	timeout := binding.Descriptor.TimeoutMs
	if timeout == 0 {
		timeout = sdk.BudgetDefaultTimeoutMs
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	lease, err := binding.acquire(ctx, m)
	if err != nil {
		return sdk.BudgetPolicyResult{}, ErrBudgetPolicyUnavailable
	}
	defer lease.Close()
	raw, err := lease.Call(ctx, method, sdk.Scope{}, request, nil)
	if err != nil {
		return sdk.BudgetPolicyResult{}, ErrBudgetPolicyUnavailable
	}
	if err := checkContribution(ctx, m, binding, declared); err != nil {
		return sdk.BudgetPolicyResult{}, ErrBudgetPolicyUnavailable
	}
	return sdk.DecodeBudgetResult(method, request, raw)
}
