package gateway

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/budget"
	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type budgetExtensionManagerKey struct{}

const budgetPolicyRefusalReason = "the selected gateway budget policy temporarily limits this request"

type budgetPolicyRefusal struct {
	reset   time.Time
	keyWide bool
}

func (r budgetPolicyRefusal) Error() string      { return budgetPolicyRefusalReason }
func (r budgetPolicyRefusal) KeyWide() bool      { return r.keyWide }
func (r budgetPolicyRefusal) ResetAt() time.Time { return r.reset }
func (r budgetPolicyRefusal) RetryAfter(now time.Time) int {
	return max(1, int((r.reset.Sub(now)+time.Second-1)/time.Second))
}

func hasScopedBudget(who access.Identity) bool {
	return slices.ContainsFunc(who.Limits(), (*access.Limit).Scoped) ||
		who.Issuer != nil && slices.ContainsFunc(who.Issuer.Limits(), (*access.Limit).Scoped)
}

// The backing limits stay on the host. Selection and the callback can only
// tighten limitations whose issuing key/scope the host already computed.
func applyBudgetPolicy(ctx context.Context, who access.Identity, method string, size int64, model string, now time.Time) ([]access.SpentScope, error) {
	cfg, err := settings.SharedChecked()
	if err != nil {
		return nil, extensionhost.ErrBudgetPolicyUnavailable
	}
	selection := cfg.GatewayBudgetPolicy
	if selection == "" {
		return nil, nil
	}
	manager, _ := ctx.Value(budgetExtensionManagerKey{}).(*extensionhost.Manager)
	if manager == nil {
		binding, _ := ctx.Value(protocolRegistryCtx{}).(protocolBinding)
		if binding.server != nil {
			manager = binding.server.extensions
		}
	}
	if manager == nil {
		return nil, extensionhost.ErrBudgetPolicyUnavailable
	}
	request := sdk.BudgetPolicyRequest{Time: now.UnixMilli(), Scopes: []sdk.BudgetScope{}}
	if method == sdk.MethodBudgetReserve {
		request.BodyBytes = &size
	}
	var limits []*access.Limit
	appendScopes := func(owner string, key access.Identity) {
		for _, limit := range key.Limits() {
			if method == sdk.MethodBudgetSpent && !limit.Scoped() ||
				method == sdk.MethodBudgetReserve && !budget.PolicyScopeMatches(limit, model) {
				continue
			}
			limits = append(limits, limit)
			request.Scopes = append(request.Scopes, sdk.BudgetScope{
				Owner: owner, Scoped: limit.Scoped(), Tokens: strconv.FormatInt(limit.Tokens, 10), CostUSD: limit.Cost,
			})
		}
	}
	appendScopes("key", who)
	if who.Issuer != nil {
		appendScopes("issuer", *who.Issuer)
	}
	if method == sdk.MethodBudgetSpent && len(limits) == 0 {
		return nil, nil
	}
	result, err := manager.EvaluateBudget(ctx, selection, method, request)
	if err != nil {
		return nil, extensionhost.ErrBudgetPolicyUnavailable
	}
	current, err := settings.SharedChecked()
	if err != nil || current.GatewayBudgetPolicy != selection || ctx.Err() != nil {
		return nil, extensionhost.ErrBudgetPolicyUnavailable
	}
	var spent []access.SpentScope
	for _, r := range result.Restrictions {
		limit := limits[r.Scope-1]
		reset := now.Add(time.Duration(r.RetrySeconds) * time.Second)
		if method == sdk.MethodBudgetReserve {
			return nil, budgetPolicyRefusal{reset: reset, keyWide: !limit.Scoped()}
		}
		spent = append(spent, access.SpentScope{Models: budget.PolicyScopeModels(limit), Reset: reset, Why: budgetPolicyRefusalReason})
	}
	return spent, nil
}
