package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var extensionAvailability requestpolicy.Availability

func extensionPolicyContext(ctx context.Context) context.Context {
	if requestpolicy.FromContext(ctx) != nil {
		return ctx
	}
	current, err := settings.SharedChecked()
	if err != nil {
		zero := 0
		current.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{MaxTotalRetries: &zero, MaxRetries: &zero}}
	}
	return requestpolicy.WithConfig(ctx, current.RequestPolicy)
}

// ExtensionAvailability is scoped by the same account-generation and package/
// configuration identity as model discovery. An old refusal cannot exclude a
// replacement account, another installed revision, or a separate paid model.
func (selected Provider) ExtensionAvailability(model string, now time.Time) (requestpolicy.Refusal, bool) {
	if !selected.IsExtension() {
		return requestpolicy.Refusal{}, false
	}
	return extensionAvailability.Blocked(selected.Account.extension.cacheKey(), selected.OwnModelID(model), now)
}

func (binding *extensionAccount) finalRequestRefusal(ctx context.Context, scope sdk.Scope, model string, result sdk.ExecuteResult, raw json.RawMessage, published bool) (*requestpolicy.Refusal, error) {
	final, err := extensionhost.DecodeFinalExecutionRefusal(binding.manifest, result, raw, model, published || extensionAttemptOf(ctx).Outcome(true) != nil)
	if err != nil || final == nil {
		return nil, err
	}
	var classification requestpolicy.Classification
	handoff := final.Facts.Phase == "admission"
	if handoff {
		classification = requestpolicy.Classification{Class: requestpolicy.Rate, Scope: "request", Rule: "local-admission"}
	} else {
		selectedPolicy, err := requestpolicy.FromContext(ctx).PolicyFor(ExtensionProviderID(scope.Plugin, scope.Provider), model, "", requestpolicy.Defaults())
		if err != nil {
			return nil, err
		}
		classification = selectedPolicy.ClassifyVendor(final.Facts, final.Rules)
	}
	observed := time.Now()
	refusal := &requestpolicy.Refusal{Classification: classification, Step: final.Step, Module: scope.Plugin, Provider: scope.Provider, Account: scope.Account, Generation: scope.Generation, Operation: scope.Operation, Attempt: scope.Attempt, Status: result.Status, Model: model, ObservedAt: observed,
		Handoff:  handoff,
		Windows:  final.Windows,
		NoEffect: final.Evidence == "rejected-before-effect" && final.Facts.Complete && final.Facts.Validate() == nil && classification.Class != requestpolicy.Unknown && classification.Scope != "unknown"}
	if !handoff && final.Facts.Complete && final.Facts.Validate() == nil {
		refusal.Observation = requestpolicy.ObserveResponse(final.Facts.Status, final.Facts.Code, final.Facts.Type)
	}
	refusal.SetTiming(final.Facts)
	if refusal.NoEffect {
		extensionAvailability.Remember(binding.cacheKey(), *refusal)
	}
	attempt := extensionAttemptOf(ctx)
	if attempt != nil {
		attempt.mu.Lock()
		attempt.refusal = refusal
		attempt.mu.Unlock()
	}
	return refusal, nil
}

// FinalRefusal returns only the current selected execution's terminal result.
// Classification callbacks never write this slot.
func (attempt *ExtensionAttempt) FinalRefusal() *requestpolicy.Refusal {
	if attempt == nil {
		return nil
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.refusal == nil {
		return nil
	}
	copy := *attempt.refusal
	return &copy
}
func (attempt *ExtensionAttempt) ClearFinalRefusal() {
	if attempt != nil {
		attempt.mu.Lock()
		attempt.refusal = nil
		attempt.mu.Unlock()
	}
}

func refusalStatus(refusal *requestpolicy.Refusal) int {
	if refusal.Status >= 400 {
		return refusal.Status
	}
	switch refusal.Class {
	case requestpolicy.Rate, requestpolicy.Quota:
		return http.StatusTooManyRequests
	case requestpolicy.Auth:
		return http.StatusUnauthorized
	case requestpolicy.Outage:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadRequest
	}
}
func refusalResponseBody(refusal *requestpolicy.Refusal) []byte {
	message := "Upstream refused the selected request (" + string(refusal.Class) + ")"
	if refusal.Handoff {
		message = "Selected route exhausted local admission capacity"
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": string(refusal.Class), "code": refusal.Rule, "message": message}})
	return body
}

func cachedRefusalResponse(ctx context.Context, request *http.Request, scope sdk.Scope, refusal requestpolicy.Refusal) *http.Response {
	extensionBegin(ctx, scope)
	refusal.Observation = requestpolicy.Observation{}
	if attempt := extensionAttemptOf(ctx); attempt != nil {
		attempt.mu.Lock()
		attempt.refusal = &refusal
		attempt.mu.Unlock()
	}
	return &http.Response{StatusCode: refusalStatus(&refusal), Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(refusalResponseBody(&refusal))), Request: request.WithContext(context.WithValue(request.Context(), extensionRefusalResponseKey{}, refusal.Observation))}
}

type extensionRefusalResponseKey struct{}

func ExtensionRefusalObservation(response *http.Response) (requestpolicy.Observation, bool) {
	if response == nil || response.Request == nil {
		return requestpolicy.Observation{}, false
	}
	observation, ok := response.Request.Context().Value(extensionRefusalResponseKey{}).(requestpolicy.Observation)
	return observation, ok
}

func FinalRequestRefusal(ctx context.Context) *requestpolicy.Refusal {
	return extensionAttemptOf(ctx).FinalRefusal()
}
func ClearFinalRequestRefusal(ctx context.Context) { extensionAttemptOf(ctx).ClearFinalRefusal() }
