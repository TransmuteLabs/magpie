package extension

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
)

const RequestPolicy Capability = "request-policy"
const MethodRequestRecover = "request.recover"

// RequestFailureFacts contains extracted response facts, never a raw body or
// credentials. Complete is false after truncation or malformed envelope fields.
type RequestFailureFacts struct {
	Status  int               `json:"status"`
	Code    string            `json:"code,omitempty"`
	Type    string            `json:"type,omitempty"`
	Message string            `json:"message,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Typed vendor body hints, combined with HTTP hints as minimum delays.
	RetryAfterMS *int64 `json:"retryAfterMs,omitempty"`
	ResetAt      string `json:"resetAt,omitempty"`
	Operation    string `json:"operation"`
	Phase        string `json:"phase"`
	Complete     bool   `json:"complete"`
}

type RequestFailureRule struct {
	ID        string            `json:"id"`
	Status    []int             `json:"status,omitempty"`
	Code      string            `json:"code,omitempty"`
	Type      string            `json:"type,omitempty"`
	Contains  string            `json:"contains,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Operation string            `json:"operation,omitempty"`
	Phase     string            `json:"phase,omitempty"`
	Class     string            `json:"class"`
	Scope     string            `json:"scope"`
}

type RequestRecoveryRequest struct {
	Step  string               `json:"step"`
	Facts RequestFailureFacts  `json:"facts"`
	Rules []RequestFailureRule `json:"rules"`
	// Evidence is "read-only" or "rejected-before-effect". A classification
	// rule is not evidence; the adapter must establish this separately.
	Evidence string `json:"evidence"`
}

// RequestFailureWindow uses the same exact Models/NotModels applicability as
// UsageWindow, scoped to the selected account generation. ID is not a global
// bucket key. Omitted timing remains unknown.
type RequestFailureWindow struct {
	ID           string   `json:"id"`
	Models       []string `json:"models,omitempty"`
	NotModels    []string `json:"notModels,omitempty"`
	RetryAfterMS *int64   `json:"retryAfterMs,omitempty"`
	ResetAt      string   `json:"resetAt,omitempty"`
}

// RequestRefusal describes the final response of an execution which never
// started its business effect. It is not a prior request.recover diagnostic.
// Return it with Outcome not-started and no accepted/output events.
type RequestRefusal struct {
	Step     string                 `json:"step"`
	Facts    RequestFailureFacts    `json:"facts"`
	Rules    []RequestFailureRule   `json:"rules"`
	Evidence string                 `json:"evidence"`
	Windows  []RequestFailureWindow `json:"windows,omitempty"`
}

// RefusedExecution keeps the actual refused response facts attached to the
// terminal result. The host still validates effect state and selected scope.
func RefusedExecution(failure RequestRecoveryRequest, windows []RequestFailureWindow) ExecuteResult {
	return ExecuteResult{Status: failure.Facts.Status, Outcome: "not-started", Refusal: &RequestRefusal{Step: failure.Step, Facts: failure.Facts, Rules: failure.Rules, Evidence: failure.Evidence, Windows: windows}}
}

type RequestRecoveryResult struct {
	Step            string `json:"step"`
	Action          string `json:"action"`
	TotalRetries    int    `json:"totalRetries"`
	ProviderRetries int    `json:"providerRetries"`
	Retry           bool   `json:"retry"`
	Reason          string `json:"reason"`
	Class           string `json:"class"`
	Scope           string `json:"scope"`
	Rule            string `json:"rule"`
	Attempt         int    `json:"attempt"`
	NextAllowedAt   string `json:"nextAllowedAt,omitempty"`
}

type RequestPolicyHost interface {
	HostCall(context.Context, string, any, any) error
}

// DecideRequest classifies every refusal, including those with no retry-safe
// evidence. An empty Evidence yields classification only. A retry permit is
// returned after the host's cancellable wait, never as a module-owned timer.
func DecideRequest(ctx context.Context, host RequestPolicyHost, request RequestRecoveryRequest) (RequestRecoveryResult, error) {
	var result RequestRecoveryResult
	if ctx == nil || host == nil {
		return result, errors.New("request decision requires its host and context")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	err := host.HostCall(ctx, MethodRequestRecover, request, &result)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return RequestRecoveryResult{}, err
	}
	if result.Retry && !validRequestRecoveryPermit(request, result) {
		return RequestRecoveryResult{}, &Error{Code: "invalid", Message: "host returned an invalid request retry permit"}
	}
	return result, nil
}

func validRequestRecoveryPermit(request RequestRecoveryRequest, result RequestRecoveryResult) bool {
	switch result.Reason {
	case "unsafe", "revoked", "canceled", "budget-exhausted", "auth-recovery-unavailable",
		"not-retryable", "attempt-limit", "invalid-random", "delay-limit", "short-wait-exceeded",
		"selected-scope-refused", "budget-unavailable", "policy-unavailable", "step-limit", "no-eligible-account":
		return false
	}
	switch result.Class {
	case "auth-refusal", "request-refusal", "content-refusal", "unknown":
		return false
	}
	facts := request.Facts
	if !ValidID(request.Step) || result.Step != request.Step || result.Action != "wait" ||
		result.Scope == "unknown" || result.Class == "quota" && result.Scope == "request" ||
		!facts.Complete || facts.Operation != MethodProviderExecute && facts.Operation != MethodToolCall && facts.Operation != MethodModuleCall ||
		facts.Phase != "response" ||
		facts.Status < 200 || facts.Status > 599 || facts.Status >= 300 && facts.Status < 400 ||
		facts.Status == http.StatusCreated || facts.Status == http.StatusAccepted || facts.Status == http.StatusPartialContent ||
		request.Evidence != "read-only" && request.Evidence != "rejected-before-effect" {
		return false
	}
	for _, count := range []int{result.TotalRetries, result.ProviderRetries, result.Attempt} {
		if count < 0 || int64(count) > 1<<53-1 {
			return false
		}
	}
	return result.ProviderRetries <= result.TotalRetries
}

type requestPolicyBody struct {
	io.ReadCloser
	read   atomic.Bool
	closed atomic.Bool
}

func (body *requestPolicyBody) Read(buffer []byte) (int, error) {
	if body.ReadCloser != http.NoBody {
		body.read.Store(true)
	}
	return body.ReadCloser.Read(buffer)
}

func (body *requestPolicyBody) Close() error {
	if body.closed.Swap(true) {
		return nil
	}
	return body.ReadCloser.Close()
}

func (body *requestPolicyBody) preserved(response *http.Response) bool {
	return response.Body != nil && (response.Body != body || !body.read.Load() && !body.closed.Load())
}

func (body *requestPolicyBody) closeResponse(response *http.Response) {
	body.Close()
	if response.Body != nil && response.Body != body {
		response.Body.Close()
	}
}

// RequestWithPolicy repeats only this HTTP request after a host permit. It has
// no retry counter, timer, account selector or credential refresh manager.
// classify must preserve the response body for the caller when retry is denied.
// Use a RoundTripper with redirect forwarding disabled; redirects are returned
// to the caller. Transport errors are returned without repeating business work.
// API 1.12 does not recover credentials: auth refusals are terminal here.
func RequestWithPolicy(ctx context.Context, host RequestPolicyHost, step string, request *http.Request, transport http.RoundTripper,
	classify func(*http.Response) (RequestRecoveryRequest, error),
) (*http.Response, *RequestRecoveryResult, error) {
	if ctx == nil || host == nil || request == nil || request.URL == nil || transport == nil || classify == nil || !ValidID(step) {
		return nil, nil, errors.New("request policy requires a named request, host, transport and classifier")
	}
	base := request.Clone(ctx)
	current := base.Clone(ctx)
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		response, err := transport.RoundTrip(current)
		if err != nil {
			if response != nil && response.Body != nil {
				response.Body.Close()
			}
			return nil, nil, err
		}
		if response == nil {
			return nil, nil, errors.New("request transport returned no response")
		}
		if response.Body == nil {
			response.Body = http.NoBody
		}
		if response.StatusCode < 200 || response.StatusCode > 599 ||
			response.Request != nil && (response.Request.URL == nil ||
				response.Request.URL.String() != current.URL.String() || response.Request.Method != current.Method) {
			response.Body.Close()
			return nil, nil, &Error{Code: "invalid", Message: "request transport returned an invalid or redirected response"}
		}
		body := &requestPolicyBody{ReadCloser: response.Body}
		response.Body = body
		failure, err := classify(response)
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && !body.preserved(response) {
			err = &Error{Code: "invalid", Message: "request classifier did not preserve the response body"}
		}
		if err != nil {
			body.closeResponse(response)
			return nil, nil, err
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			return response, nil, nil
		}
		if failure.Facts.Operation == "" {
			return response, nil, nil
		}
		if !failure.Facts.Complete || base.Body != nil && base.GetBody == nil {
			failure.Evidence = ""
		}
		failure.Step = step
		failure.Facts.Status = response.StatusCode
		decision, err := DecideRequest(ctx, host, failure)
		if err == nil && !body.preserved(response) {
			err = &Error{Code: "invalid", Message: "request classifier did not preserve the response body"}
		}
		if err != nil {
			body.closeResponse(response)
			return nil, nil, err
		}
		if !decision.Retry {
			return response, &decision, nil
		}
		body.closeResponse(response)
		current = base.Clone(ctx)
		if base.Body != nil {
			current.Body, err = base.GetBody()
			if err != nil {
				return nil, nil, err
			}
		}
	}
}
