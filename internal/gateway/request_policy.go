package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
)

func withRequestPolicy(ctx context.Context) context.Context {
	current, err := settings.SharedChecked()
	if err != nil {
		zero := 0
		return requestpolicy.WithConfig(ctx, &requestpolicy.Config{Defaults: requestpolicy.Options{MaxRetries: &zero, MaxTotalRetries: &zero}})
	}
	return requestpolicy.WithConfig(ctx, current.RequestPolicy)
}

func requestPolicy(ctx context.Context, providerID string) (requestpolicy.Policy, error) {
	return resolveRequestPolicy(ctx, providerID, passingPolicyDefaults())
}

func passingPolicyDefaults() requestpolicy.Policy {
	base := requestpolicy.Defaults()
	base.BaseDelay = retryPause
	base.MaxDelay = longestPause
	return base
}

func resolveRequestPolicy(ctx context.Context, providerID string, base requestpolicy.Policy) (requestpolicy.Policy, error) {
	if budget := requestpolicy.FromContext(ctx); budget != nil {
		return budget.PolicyFor(providerID, requestpolicy.SelectedModel(ctx), "", base)
	}
	current, err := settings.SharedChecked()
	if err != nil {
		return requestpolicy.Policy{}, err
	}
	return requestpolicy.ResolveFor(current.RequestPolicy, providerID, requestpolicy.SelectedModel(ctx), "", base)
}

// errorEnvelope rejects duplicate keys and trailing JSON instead of silently
// choosing one of two conflicting machine-code fields.
func errorEnvelope(body []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected error object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid error field")
		}
		if _, exists := fields[name]; exists {
			return nil, errors.New("duplicate error field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing error data")
	}
	return fields, nil
}

func responseFacts(status int, headers http.Header, body []byte) requestpolicy.Facts {
	facts := requestpolicy.Facts{Status: status, Operation: "inference", Phase: "response", Headers: map[string]string{}, Complete: len(body) <= 64*1024 && utf8.Valid(body)}
	// Only retry timing headers enter this legacy classifier. SDK adapters supply
	// their separately bounded, provider-specific header facts to the same matcher.
	for name, values := range headers {
		normalized := strings.ToLower(name)
		if normalized != "retry-after" && !(strings.Contains(normalized, "ratelimit") && strings.Contains(normalized, "reset")) {
			continue
		}
		if len(values) == 0 {
			continue
		}
		if len(facts.Headers) >= 16 || len(name) > 64 || len(values[0]) > 300 {
			facts.Complete = false
			continue
		}
		facts.Headers[name] = values[0]
	}
	trimmed := bytes.TrimSpace(body)
	if !facts.Complete {
		return facts
	}
	if len(trimmed) == 0 {
		return facts
	}
	if trimmed[0] != '{' {
		// Plain-text/HTML proxy errors remain legacy facts. A malformed JSON array
		// or scalar cannot stand in for the expected machine error object.
		facts.Complete = len(trimmed) <= 1024 && !bytes.ContainsAny(trimmed[:1], "[\"-0123456789") && !json.Valid(trimmed)
		facts.Message = string(trimmed[:min(len(trimmed), 1024)])
		return facts
	}
	fields, err := errorEnvelope(trimmed)
	if err != nil {
		facts.Complete = false
		return facts
	}
	emptyEnvelope := len(fields) == 0
	if nested, exists := fields["error"]; exists {
		onlyError := len(fields) == 1
		fields, err = errorEnvelope(nested)
		if err != nil {
			facts.Complete = false
			return facts
		}
		emptyEnvelope = onlyError && len(fields) == 0
	}
	for name, destination := range map[string]*string{"code": &facts.Code, "type": &facts.Type, "message": &facts.Message} {
		value, exists := fields[name]
		if !exists {
			continue
		}
		if name == "code" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			continue
		}
		if err := json.Unmarshal(value, destination); err == nil && !bytes.Equal(value, []byte("null")) {
			continue
		}
		if name == "code" && len(value) > 0 && (value[0] == '-' || value[0] >= '0' && value[0] <= '9') {
			var number json.Number
			if json.Unmarshal(value, &number) == nil {
				*destination = number.String()
				continue
			}
		}
		facts.Complete = false
	}
	if facts.Code == "" && facts.Type == "" && facts.Message == "" {
		facts.Complete = emptyEnvelope
	}
	if len(facts.Code) > 256 || len(facts.Type) > 128 || len(facts.Message) > 1024 {
		facts.Complete = false
	}
	facts.Code = facts.Code[:min(len(facts.Code), 256)]
	facts.Type = facts.Type[:min(len(facts.Type), 128)]
	facts.Message = facts.Message[:min(len(facts.Message), 1024)]
	return facts
}

func passingFor(ctx context.Context, providerID string, status int, headers http.Header, body []byte, again int) (time.Duration, bool) {
	return passingForResponse(ctx, providerID, status, headers, body, again, false)
}

func passingForResponse(ctx context.Context, providerID string, status int, headers http.Header, body []byte, again int, qwenBusy bool) (time.Duration, bool) {
	if routedThere(headers) || provider.ExtensionOutcome(ctx, true) != nil {
		return 0, false
	}
	base := passingPolicyDefaults()
	if qwenBusy {
		base.BaseDelay = jittered(base.BaseDelay)
	}
	policy, err := resolveRequestPolicy(ctx, providerID, base)
	if err != nil {
		return 0, false
	}
	// Legacy routing's classifier also owns cooldowns and account/model
	// fallback. Until that complete path delegates classification, only the
	// shared scheduling settings apply here. Declarative response rules belong
	// to negotiated transport callbacks, not a conflicting retry-only view.
	policy.Rules = nil
	classification := requestpolicy.Classification{Class: requestpolicy.Unknown, Scope: "request", Rule: "gateway.unknown"}
	safe := false
	switch {
	case qwenBusy:
		classification.Class, classification.Scope, classification.Rule, safe = requestpolicy.Rate, "account", "gateway.qwen-plan-busy", true
	case busyStatus(status):
		if bodyHas(body, didNotStream) || bodyHas(body, cutVoidWords) {
			return 0, false
		}
		if bodyHas(body, emptyReply) {
			if again >= 1 {
				return 0, false
			}
		}
		classification.Class, classification.Rule, safe = requestpolicy.Outage, "gateway.outage", true
	case status == 429:
		if failure(status, body) == failRate && !creditWords.Match(wordsOf(body)) && !walled(body) {
			classification.Class, classification.Scope, classification.Rule, safe = requestpolicy.Rate, "account", "gateway.rate", true
		} else {
			classification.Class, classification.Scope, classification.Rule = requestpolicy.Quota, "account", "gateway.quota"
		}
	case regionRefused(status, wordsOf(body)):
		classification.Class, classification.Rule, safe = requestpolicy.Outage, "gateway.region", true
	}
	facts := responseFacts(status, headers, body)
	var decision requestpolicy.Decision
	if budget := requestpolicy.FromContext(ctx); budget != nil {
		decision = budget.Decide(policy, facts, classification, requestpolicy.Safety{NoEffect: safe}, again)
	} else {
		decision = policy.Decide(facts, classification, requestpolicy.Safety{NoEffect: safe}, again, time.Now(), 0)
	}
	return decision.Delay, decision.Retry
}

// Admission is separate from probing a decision: route planning asks whether
// a retry is possible more than once, but only the chosen retry spends budget.
func admitRequestRetry(ctx context.Context, providerID string, delay time.Duration) bool {
	base := requestpolicy.Defaults()
	// Passing has its existing 8s schedule cap. Patience is a separate routing
	// workflow and can wait longer; only an explicit policy cap narrows it.
	base.MaxDelay = longestWait
	policy, err := resolveRequestPolicy(ctx, providerID, base)
	if err != nil || policy.MaxRetries != nil && *policy.MaxRetries == 0 || provider.ExtensionOutcome(ctx, true) != nil {
		return false
	}
	budget := requestpolicy.FromContext(ctx)
	return budget != nil && (policy.MaxRetries == nil || budget.Attempts(ctx, providerID) < *policy.MaxRetries) && budget.Reserve(ctx, policy, requestpolicy.Decision{Retry: true, Delay: delay}) == nil
}

// Each queued repeat spends its own provider permit and the same root budget.
// Reservations are conservative: a later candidate which is never dispatched
// does not refund a permit and cannot replenish a nested retry budget.
func admitReplannedRequests(ctx context.Context, candidates []candidate, delay time.Duration) []candidate {
	admitted := make([]candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if admitRequestRetry(requestpolicy.WithSelection(ctx, candidate.restID(), candidate.p.OwnModelID(candidate.model)), candidate.p.ID, delay) {
			admitted = append(admitted, candidate)
		}
	}
	return admitted
}

// Policy never reorders candidates: Smart, order and other routing owners have
// already selected this list. Check availability and immediate admission at the destination.
func nextPolicyCandidate(ctx context.Context, candidates []candidate, start int) int {
	for index := start; index < len(candidates); index++ {
		selected := candidates[index]
		if _, blocked := selected.p.ExtensionAvailability(selected.model, time.Now()); blocked {
			continue
		}
		if _, resting := restingOf(selected); resting {
			continue
		}
		destination := requestpolicy.WithSelection(ctx, selected.restID(), selected.p.OwnModelID(selected.model))
		if budget := requestpolicy.FromContext(destination); budget != nil {
			policy, err := requestPolicy(destination, selected.p.ID)
			if err != nil || !budget.CanDispatch(destination, policy) {
				continue
			}
		}
		if selected.p.CheckExecutor(ctx) != nil {
			continue
		}
		return index
	}
	return -1
}

func policyStatus(refusal *requestpolicy.Refusal) int {
	if refusal.Status >= 400 {
		return refusal.Status
	}
	switch refusal.Class {
	case requestpolicy.Rate, requestpolicy.Quota:
		return 429
	case requestpolicy.Auth:
		return 401
	case requestpolicy.Outage:
		return 503
	default:
		return 400
	}
}

// resolveFinalPolicy consumes only a final refusal published by the provider
// bridge. Callback diagnostics never enter routing or availability decisions.
func resolveFinalPolicy(ctx context.Context, selected candidate, refusal *requestpolicy.Refusal, hasNext bool, wait func(context.Context, time.Duration) error) requestpolicy.Decision {
	budget := requestpolicy.FromContext(ctx)
	terminal := requestpolicy.Decision{Classification: refusal.Classification, Action: requestpolicy.ActionTerminal, Reason: "policy-unavailable", NextAllowed: refusal.NextAllowed}
	if budget == nil || provider.ExtensionOutcome(ctx, true) != nil {
		return terminal
	}
	policy, err := budget.PolicyFor(selected.p.ID, selected.p.OwnModelID(selected.model), "", requestpolicy.Defaults())
	if err != nil {
		return terminal
	}
	// Every applicable window contributes a lower bound. Scoped overrides can
	// narrow the wait/count limits without creating a second candidate order.
	for index, window := range refusal.Windows {
		windowPolicy, err := budget.PolicyFor(selected.p.ID, selected.p.OwnModelID(selected.model), window.ID, requestpolicy.Defaults())
		if err != nil {
			return terminal
		}
		if index == 0 {
			policy = windowPolicy
			continue
		}
		policy.ShortWait = min(policy.ShortWait, windowPolicy.ShortWait)
		policy.MaxDelay = min(policy.MaxDelay, windowPolicy.MaxDelay)
		policy.MaxElapsed = min(policy.MaxElapsed, windowPolicy.MaxElapsed)
		policy.MaxTotalRetries = min(policy.MaxTotalRetries, windowPolicy.MaxTotalRetries)
		if windowPolicy.MaxRetries != nil && (policy.MaxRetries == nil || *windowPolicy.MaxRetries < *policy.MaxRetries) {
			policy.MaxRetries = windowPolicy.MaxRetries
		}
	}
	// Classification was resolved from the actual response before raw text was
	// discarded. Scheduling must not reclassify a reconstructed empty message.
	policy.Rules = nil
	decision := budget.Disposition(policy, refusal.Facts(time.Now()), refusal.Classification, requestpolicy.Safety{NoEffect: refusal.NoEffect}, budget.Attempts(ctx, selected.p.ID))
	if deadline, bounded := ctx.Deadline(); bounded && decision.Action == requestpolicy.ActionWait && !time.Now().Add(decision.Delay).Before(deadline) {
		// The selected account cannot become ready within this request. A ready
		// sibling can still use the unspent permit without waiting for its reset.
		decision.Retry = false
		decision.Action = requestpolicy.ActionNext
		decision.Reason = "deadline-limit"
	}
	if decision.Action == requestpolicy.ActionWait {
		if err := budget.Reserve(ctx, policy, decision); err != nil {
			decision.Retry = false
			decision.Action = requestpolicy.ActionTerminal
			decision.Reason = "budget-exhausted"
			if hasNext {
				decision.Action = requestpolicy.ActionNext
				decision.Reason = "wait-budget-exceeded"
			}
		} else {
			if wait == nil {
				wait = requestpolicy.Wait
			}
			if err := wait(ctx, decision.Delay); err != nil {
				decision.Retry = false
				decision.Action = requestpolicy.ActionTerminal
				decision.Reason = "canceled"
			}
		}
	}
	if decision.Action == requestpolicy.ActionNext && !hasNext {
		decision.Action = requestpolicy.ActionTerminal
		decision.Reason = "no-eligible-account"
	}
	total, providerCount := budget.Counts(selected.p.ID)
	log.Printf("request policy final: step=%q action=%q provider=%q account=%q generation=%d operation=%q model=%q class=%q scope=%q rule=%q nextAllowedAt=%q reason=%q totalRetries=%d providerRetries=%d", refusal.Step, decision.Action, selected.p.ID, refusal.Account, refusal.Generation, refusal.Operation, selected.model, decision.Class, decision.Scope, decision.Rule, formatPolicyTime(decision.NextAllowed), decision.Reason, total, providerCount)
	return decision
}
func formatPolicyTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
func policyFailureHeaders(writer http.ResponseWriter, decision requestpolicy.Decision) {
	writer.Header().Set("X-Should-Retry", "false")
	if remaining := time.Until(decision.NextAllowed); remaining > 0 {
		writer.Header().Set("Retry-After", strconv.FormatInt(int64((remaining+time.Second-1)/time.Second), 10))
	}
}

func policyMemberCandidates(members []provider.Member) []candidate {
	candidates := make([]candidate, len(members))
	for index, member := range members {
		candidates[index] = candidateOf(member.Provider, member.Model)
	}
	return candidates
}
func finalPolicyMessage(refusal *requestpolicy.Refusal) string {
	return "Upstream refused the selected request (" + string(refusal.Class) + ")"
}
