package extension

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type requestPolicyKey struct{}
type requestPolicyInvocation struct {
	mu     sync.Mutex
	lease  *Lease
	scope  sdk.Scope
	method string
	model  string
	output bool
	steps  map[string]int
}

var errRequestPolicyOutput = errors.New("request output already published")

func beginRequestPolicy(ctx context.Context, lease *Lease, scope sdk.Scope, method string, params any) context.Context {
	if !lease.worker.entry.Manifest.Contract.Has(sdk.RequestPolicy) {
		return ctx
	}
	var selected struct {
		Model string `json:"model"`
	}
	if method == sdk.MethodProviderExecute {
		raw, err := json.Marshal(params)
		if err != nil || json.Unmarshal(raw, &selected) != nil {
			return ctx
		}
	}
	if requestpolicy.FromContext(ctx) == nil {
		current, err := settings.SharedChecked()
		if err != nil {
			zero := 0
			current.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{MaxRetries: &zero, MaxTotalRetries: &zero}}
		}
		ctx = requestpolicy.WithConfig(ctx, current.RequestPolicy)
	}
	return context.WithValue(ctx, requestPolicyKey{}, &requestPolicyInvocation{lease: lease, scope: scope, method: method, model: selected.Model, steps: make(map[string]int)})
}

func observeRequestOutput(ctx context.Context, event Event) {
	if event.Name == "accepted" {
		return
	}
	if invocation, ok := ctx.Value(requestPolicyKey{}).(*requestPolicyInvocation); ok {
		invocation.mu.Lock()
		invocation.output = true
		invocation.mu.Unlock()
	}
}

func (m *Manager) checkRequestPolicy(ctx context.Context, invocation *requestPolicyInvocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := netproxy.Held(ctx); err != nil {
		return err
	}
	if err := checkWindowAdmission(ctx); err != nil {
		return err
	}
	lease := invocation.lease
	if lease.closed.Load() || lease.ctx.Err() != nil {
		return errors.New("request policy lease was revoked")
	}
	if _, err := m.authFor(invocation.scope); err != nil {
		return err
	}
	current, err := m.readModuleConfig(invocation.scope.Plugin)
	if err != nil || current.Version != lease.ConfigVersion() {
		return errors.New("request policy configuration changed")
	}
	entries, err := m.List()
	if err != nil || !slices.ContainsFunc(entries, func(entry Installed) bool {
		return entry.Manifest.ID == invocation.scope.Plugin && !entry.Disabled && entry.Revision == lease.Revision() &&
			entry.observerEnablementErr == nil && entry.EnableGeneration == lease.worker.entry.EnableGeneration
	}) {
		return errors.New("request policy package changed")
	}
	invocation.mu.Lock()
	defer invocation.mu.Unlock()
	if invocation.output {
		return errRequestPolicyOutput
	}
	return nil
}

func requestPolicyFailureReason(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "canceled"
	}
	if errors.Is(err, errRequestPolicyOutput) {
		return "unsafe"
	}
	return "revoked"
}

func (manager *Manager) requestSafety(invocation *requestPolicyInvocation, evidence string, complete bool) requestpolicy.Safety {
	safety := requestpolicy.Safety{NoEffect: complete && evidence != ""}
	invocation.mu.Lock()
	safety.OutputStarted = invocation.output
	invocation.mu.Unlock()
	if evidence != "rejected-before-effect" {
		return safety
	}
	operation, err := manager.store.Operation(invocation.scope.Plugin, invocation.scope.Operation)
	if err != nil || operation.Scope != invocation.scope {
		safety.Uncertain = true
		return safety
	}
	switch operation.State {
	case "dispatched":
	case "accepted":
		safety.Accepted = true
	default:
		safety.Uncertain = true
	}
	return safety
}

func (m *Manager) recoverRequest(ctx context.Context, scope sdk.Scope, raw json.RawMessage, authority hostAuthority) (any, error) {
	invocation, ok := ctx.Value(requestPolicyKey{}).(*requestPolicyInvocation)
	if !ok || invocation.scope != scope || invocation.method != authority.method ||
		authority.manifest.Contract.Version.Minor < 12 || !authority.manifest.Contract.Has(sdk.RequestPolicy) ||
		(scope.Provider == "" || authority.method != sdk.MethodProviderExecute && authority.method != sdk.MethodToolCall && authority.method != sdk.MethodModuleCall) {
		return nil, errors.New("request recovery requires its negotiated selected invocation")
	}
	var request struct {
		Step     string               `json:"step"`
		Facts    requestpolicy.Facts  `json:"facts"`
		Rules    []requestpolicy.Rule `json:"rules"`
		Evidence string               `json:"evidence"`
	}
	if !decodeHostParams(raw, &request) || !sdk.ValidID(request.Step) || len(request.Step) > 128 || request.Facts.Validate() != nil ||
		request.Facts.Operation != authority.method || request.Facts.Phase != "response" || len(request.Rules) > 64 ||
		request.Evidence != "" && request.Evidence != "read-only" && request.Evidence != "rejected-before-effect" {
		return nil, errors.New("invalid bounded request recovery evidence")
	}
	budget := requestpolicy.FromContext(ctx)
	// This is the same stable identity as provider.ExtensionProviderID. Keeping
	// the digest here avoids importing the provider owner into its runtime.
	digest := sha256.Sum256([]byte(scope.Plugin + "\x00" + scope.Provider))
	providerID := "extension-" + hex.EncodeToString(digest[:12])
	result := sdk.RequestRecoveryResult{Step: request.Step, Action: requestpolicy.ActionTerminal, Reason: "revoked", Class: string(requestpolicy.Unknown), Scope: "unknown"}
	finishResult := func() sdk.RequestRecoveryResult {
		if budget != nil {
			result.TotalRetries, result.ProviderRetries = budget.Counts(providerID)
		}
		return result
	}
	defer func() { m.recordRequestPolicy(scope, finishResult()) }()
	if err := m.checkRequestPolicy(ctx, invocation); err != nil {
		result.Reason = requestPolicyFailureReason(ctx, err)
		return nil, err
	}
	if budget == nil {
		result.Reason = "budget-unavailable"
		return nil, errors.New("request policy budget is unavailable")
	}
	policy, err := budget.PolicyFor(providerID, invocation.model, "", requestpolicy.Defaults())
	if err != nil {
		result.Reason = "policy-unavailable"
		return nil, err
	}
	classification := policy.ClassifyVendor(request.Facts, request.Rules)
	invocation.mu.Lock()
	attempt, exists := invocation.steps[request.Step]
	if !exists && len(invocation.steps) >= 64 {
		invocation.mu.Unlock()
		result.Reason = "step-limit"
		return nil, errors.New("too many request policy steps")
	}
	invocation.steps[request.Step] = attempt + 1
	invocation.mu.Unlock()
	// Classification is not evidence. Unknown/incomplete envelopes cannot
	// authorize a repeat, regardless of an operator's configured retry count.
	decision := budget.Disposition(policy, request.Facts, classification, m.requestSafety(invocation, request.Evidence, request.Facts.Complete), attempt)
	if classification.Class == requestpolicy.Auth {
		decision.Reason = "auth-recovery-unavailable"
	}
	result = sdk.RequestRecoveryResult{Step: request.Step, Action: decision.Action, Retry: false, Reason: decision.Reason, Class: string(decision.Class), Scope: decision.Scope, Rule: decision.Rule, Attempt: attempt}
	if !decision.NextAllowed.IsZero() {
		result.NextAllowedAt = decision.NextAllowed.UTC().Format(time.RFC3339Nano)
	}
	if !decision.Retry {
		if result.Action == requestpolicy.ActionNext {
			result.Action = requestpolicy.ActionTerminal
		}
		return finishResult(), nil
	}
	if err := budget.Wait(ctx, policy, decision, nil); err != nil {
		if ctx.Err() != nil {
			result.Action = requestpolicy.ActionTerminal
			result.Reason = "canceled"
			return nil, ctx.Err()
		}
		result.Reason = "budget-exhausted"
		result.Action = requestpolicy.ActionTerminal
		return finishResult(), nil
	}
	if err := m.checkRequestPolicy(ctx, invocation); err != nil {
		result.Action = requestpolicy.ActionTerminal
		result.Reason = requestPolicyFailureReason(ctx, err)
		return nil, err
	}
	if safety := m.requestSafety(invocation, request.Evidence, request.Facts.Complete); safety.Accepted || safety.Uncertain || safety.OutputStarted {
		result.Action = requestpolicy.ActionTerminal
		result.Reason = "unsafe"
		return finishResult(), nil
	}
	result.Retry = true
	result.Action = requestpolicy.ActionWait
	return finishResult(), nil
}

func (m *Manager) recordRequestPolicy(scope sdk.Scope, result sdk.RequestRecoveryResult) {
	m.mu.Lock()
	sink := m.diagnosticLog
	m.mu.Unlock()
	if sink != nil {
		sink(moduleDiagnostic{Plugin: scope.Plugin, Provider: scope.Provider, Account: scope.Account, Operation: scope.Operation, Attempt: scope.Attempt, RequestPolicy: &result})
		return
	}
	log.Printf("extension request policy: module=%q provider=%q account=%q generation=%d operation=%q step=%q action=%q class=%q scope=%q rule=%q nextAllowedAt=%q reason=%q attempt=%d totalRetries=%d providerRetries=%d retry=%t",
		scope.Plugin, scope.Provider, scope.Account, scope.Generation, scope.Operation, result.Step, result.Action, result.Class, result.Scope, result.Rule, result.NextAllowedAt, result.Reason, result.Attempt, result.TotalRetries, result.ProviderRetries, result.Retry)
}
