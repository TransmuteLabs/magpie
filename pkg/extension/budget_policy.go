package extension

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
)

const (
	BudgetPolicy           Capability = "budget-policy"
	MethodBudgetReserve               = "budget.reserve"
	MethodBudgetSpent                 = "budget.spent"
	BudgetDefaultTimeoutMs            = 250
	MaxBudgetRetrySeconds             = 3600
)

type BudgetPolicyDescriptor struct {
	ID        string `json:"id"`
	TimeoutMs int    `json:"timeoutMs,omitempty"`
}

// BudgetScope is a sanitized, request-local view of a host-declared limit.
// Its one-based position is the only scope reference accepted in a result.
type BudgetScope struct {
	Owner   string  `json:"owner"`
	Scoped  bool    `json:"scoped"`
	Tokens  string  `json:"tokens"`
	CostUSD float64 `json:"costUSD"`
}

type BudgetPolicyRequest struct {
	Policy    string        `json:"policy"`
	Time      int64         `json:"time"`
	BodyBytes *int64        `json:"bodyBytes,omitempty"`
	Scopes    []BudgetScope `json:"scopes"`
}

type BudgetRestriction struct {
	Scope        int `json:"scope"`
	RetrySeconds int `json:"retrySeconds"`
}

type BudgetPolicyResult struct {
	Restrictions []BudgetRestriction `json:"restrictions"`
}

func IsBudgetMethod(method string) bool {
	return method == MethodBudgetReserve || method == MethodBudgetSpent
}

func (manifest Manifest) validateBudgetPolicies() error {
	if !manifest.Contract.Has(BudgetPolicy) && len(manifest.BudgetPolicies) == 0 {
		return nil
	}
	if manifest.Component != nil || manifest.Contract.Version.Minor < 13 || !manifest.Contract.Has(BudgetPolicy) ||
		len(manifest.BudgetPolicies) == 0 || len(manifest.BudgetPolicies) > 32 {
		return errors.New("budget policies require a primary SDK API 1.13 budget-policy declaration")
	}
	seen := map[string]bool{}
	for _, descriptor := range manifest.BudgetPolicies {
		if !validID(descriptor.ID) || seen[descriptor.ID] || descriptor.TimeoutMs != 0 &&
			(descriptor.TimeoutMs < 10 || descriptor.TimeoutMs > 1000) {
			return errors.New("invalid budget policy descriptor")
		}
		seen[descriptor.ID] = true
	}
	return nil
}

func ValidateBudgetScope(scope Scope) error {
	if scope != (Scope{Plugin: scope.Plugin}) {
		return &Error{Code: "permission_denied", Message: "budget policy has no selected or write scope"}
	}
	return nil
}

// Budget policy wire keys are case-sensitive in both SDKs. Check the
// envelope and nested scopes before decoding into Go's case-folded structs.
func budgetObject(raw json.RawMessage, keys ...string) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil
	}
	for name := range fields {
		if !slices.Contains(keys, name) {
			return nil
		}
	}
	return fields
}

func budgetArrayFields(raw json.RawMessage, keys ...string) bool {
	var array []json.RawMessage
	if json.Unmarshal(raw, &array) != nil || array == nil {
		return false
	}
	for _, item := range array {
		if budgetObject(item, keys...) == nil {
			return false
		}
	}
	return true
}

func DecodeBudgetRequest(manifest Manifest, method string, raw json.RawMessage) (BudgetPolicyRequest, error) {
	bad := &Error{Code: "invalid_params", Message: "invalid sanitized budget policy request"}
	var request BudgetPolicyRequest
	if len(raw) > 4096 {
		return BudgetPolicyRequest{}, bad
	}
	fields := budgetObject(raw, "policy", "time", "bodyBytes", "scopes")
	if fields == nil || !budgetArrayFields(fields["scopes"], "owner", "scoped", "tokens", "costUSD") ||
		json.Unmarshal(raw, &request) != nil ||
		request.Time <= 0 || request.Time > 1<<53-1 || request.Scopes == nil || len(request.Scopes) > 10 ||
		!IsBudgetMethod(method) {
		return BudgetPolicyRequest{}, bad
	}
	switch method {
	case MethodBudgetReserve:
		if request.BodyBytes == nil || *request.BodyBytes < -1 || *request.BodyBytes > 1<<53-1 {
			return BudgetPolicyRequest{}, bad
		}
	case MethodBudgetSpent:
		if request.BodyBytes != nil || len(request.Scopes) == 0 {
			return BudgetPolicyRequest{}, bad
		}
	}
	found := false
	for _, p := range manifest.BudgetPolicies {
		found = found || p.ID == request.Policy
	}
	if !found {
		return BudgetPolicyRequest{}, bad
	}
	for _, scope := range request.Scopes {
		tokens, err := strconv.ParseInt(scope.Tokens, 10, 64)
		if scope.Owner != "key" && scope.Owner != "issuer" || err != nil || tokens < 0 || strconv.FormatInt(tokens, 10) != scope.Tokens ||
			!validExecutionAmount(scope.CostUSD) || tokens == 0 && scope.CostUSD == 0 ||
			method == MethodBudgetSpent && !scope.Scoped {
			return BudgetPolicyRequest{}, bad
		}
	}
	return request, nil
}

func DecodeBudgetResult(method string, request BudgetPolicyRequest, raw json.RawMessage) (BudgetPolicyResult, error) {
	bad := &Error{Code: "invalid_result", Message: "invalid budget policy result"}
	var result BudgetPolicyResult
	if !IsBudgetMethod(method) || len(raw) > 4096 {
		return BudgetPolicyResult{}, bad
	}
	fields := budgetObject(raw, "restrictions")
	if fields == nil || !budgetArrayFields(fields["restrictions"], "scope", "retrySeconds") ||
		json.Unmarshal(raw, &result) != nil || result.Restrictions == nil ||
		len(result.Restrictions) > len(request.Scopes) || method == MethodBudgetReserve && len(result.Restrictions) > 1 {
		return BudgetPolicyResult{}, bad
	}
	seen := make(map[int]bool, len(result.Restrictions))
	for _, restriction := range result.Restrictions {
		if restriction.Scope < 1 || restriction.Scope > len(request.Scopes) || seen[restriction.Scope] ||
			restriction.RetrySeconds < 1 || restriction.RetrySeconds > MaxBudgetRetrySeconds {
			return BudgetPolicyResult{}, bad
		}
		seen[restriction.Scope] = true
	}
	return result, nil
}

func SplitBudgetPolicy(selection string) (module, policy string, ok bool) {
	module, policy, found := strings.Cut(selection, "/")
	return module, policy, found && validID(module) && validID(policy)
}

func budgetPolicyHandler(manifest Manifest, method string, handler Handler) Handler {
	return func(ctx context.Context, call Call, stream *Stream) (any, error) {
		if call.Auth != nil {
			return nil, &Error{Code: "permission_denied", Message: "budget policy cannot borrow credentials"}
		}
		if err := ValidateBudgetScope(call.Scope); err != nil {
			return nil, err
		}
		req, err := DecodeBudgetRequest(manifest, method, call.Params)
		if err != nil {
			return nil, err
		}
		result, err := handler(ctx, call, stream)
		if err != nil {
			return nil, err
		}
		raw, err := marshalPayload(result)
		if err != nil {
			return nil, err
		}
		return DecodeBudgetResult(method, req, raw)
	}
}
