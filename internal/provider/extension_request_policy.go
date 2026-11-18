package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

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

func (binding *extensionAccount) finalRequestRefusal(ctx context.Context, scope sdk.Scope, model string, result sdk.ExecuteResult, raw json.RawMessage, seenHead bool) (*requestpolicy.Refusal, error) {
	if result.Refusal == nil {
		return nil, nil
	}
	if binding.manifest.Contract.Version.Minor < 12 || !binding.manifest.Contract.Has(sdk.RequestPolicy) || result.Outcome != "not-started" || seenHead || extensionAttemptOf(ctx).Outcome(true) != nil {
		return nil, errors.New("final refusal contradicted the execution boundary")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields["refusal"]) > 128*1024 {
		return nil, errors.New("invalid final refusal")
	}
	var final struct {
		Step     string                 `json:"step"`
		Facts    requestpolicy.Facts    `json:"facts"`
		Rules    []requestpolicy.Rule   `json:"rules"`
		Evidence string                 `json:"evidence"`
		Windows  []requestpolicy.Window `json:"windows"`
	}
	decoder := json.NewDecoder(bytes.NewReader(fields["refusal"]))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&final) != nil || !sdk.ValidID(final.Step) || len(final.Step) > 128 || final.Facts.Status != result.Status || final.Facts.Operation != sdk.MethodProviderExecute || final.Facts.Phase != "response" || len(final.Windows) > 64 || len(final.Rules) > 64 {
		return nil, errors.New("invalid final refusal facts")
	}
	if final.Evidence != "" && final.Evidence != "rejected-before-effect" {
		return nil, errors.New("final execution refusal requires whole-operation rejection evidence")
	}
	selectedPolicy, err := requestpolicy.FromContext(ctx).PolicyFor(ExtensionProviderID(scope.Plugin, scope.Provider), model, "", requestpolicy.Defaults())
	if err != nil {
		return nil, err
	}
	classification := selectedPolicy.ClassifyVendor(final.Facts, final.Rules)
	observed := time.Now()
	refusal := &requestpolicy.Refusal{Classification: classification, Step: final.Step, Module: scope.Plugin, Provider: scope.Provider, Account: scope.Account, Generation: scope.Generation, Operation: scope.Operation, Attempt: scope.Attempt, Status: result.Status, Model: model, ObservedAt: observed,
		NoEffect: final.Evidence == "rejected-before-effect" && final.Facts.Complete && final.Facts.Validate() == nil && classification.Class != requestpolicy.Unknown && classification.Scope != "unknown"}
	if final.Facts.Complete && final.Facts.Validate() == nil {
		refusal.Observation = requestpolicy.ObserveResponse(final.Facts.Status, final.Facts.Code, final.Facts.Type)
	}
	seen := map[string]bool{}
	for _, window := range final.Windows {
		if window.Validate() != nil || seen[window.ID] || !window.Applies(model) {
			return nil, errors.New("invalid final refusal window applicability")
		}
		seen[window.ID] = true
		refusal.Windows = append(refusal.Windows, window)
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
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": string(refusal.Class), "code": refusal.Rule, "message": "Upstream refused the selected request (" + string(refusal.Class) + ")"}})
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
