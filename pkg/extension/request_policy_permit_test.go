package extension

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func namedPermitRequest() RequestRecoveryRequest {
	return RequestRecoveryRequest{Step: "named-read", Evidence: "read-only",
		Facts: RequestFailureFacts{Status: 503, Operation: MethodModuleCall, Phase: "response", Complete: true}}
}

func namedPermitResult() RequestRecoveryResult {
	return RequestRecoveryResult{Step: "named-read", Action: "wait", Retry: true,
		TotalRetries: 2, ProviderRetries: 1, Attempt: 1}
}

type permitBody struct {
	io.Reader
	closed int
}

func (body *permitBody) Close() error {
	body.closed++
	return nil
}

func TestRequestPolicyPermitRejectsMalformedReply(t *testing.T) {
	for name, change := range map[string]func(*RequestRecoveryResult){
		"foreign-step":       func(result *RequestRecoveryResult) { result.Step = "foreign-read" },
		"empty-step":         func(result *RequestRecoveryResult) { result.Step = "" },
		"terminal-action":    func(result *RequestRecoveryResult) { result.Action = "terminal" },
		"empty-action":       func(result *RequestRecoveryResult) { result.Action = "" },
		"next-account":       func(result *RequestRecoveryResult) { result.Action = "next-account" },
		"unknown-action":     func(result *RequestRecoveryResult) { result.Action = "retry" },
		"negative-total":     func(result *RequestRecoveryResult) { result.TotalRetries = -1 },
		"negative-provider":  func(result *RequestRecoveryResult) { result.ProviderRetries = -1 },
		"provider-over-root": func(result *RequestRecoveryResult) { result.ProviderRetries = 3 },
		"negative-attempt":   func(result *RequestRecoveryResult) { result.Attempt = -1 },
		"known-unsafe":       func(result *RequestRecoveryResult) { result.Reason = "unsafe" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := namedPermitResult()
			change(&bad)
			host := &policyTestHost{decisions: []RequestRecoveryResult{bad}}
			body := &permitBody{Reader: strings.NewReader("original refusal")}
			request, _ := http.NewRequest(http.MethodGet, "https://fixture.invalid/read", nil)
			sends := 0
			response, _, err := RequestWithPolicy(t.Context(), host, "named-read", request,
				policyRoundTrip(func(*http.Request) (*http.Response, error) {
					sends++
					return &http.Response{StatusCode: 503, Body: body}, nil
				}), func(*http.Response) (RequestRecoveryRequest, error) { return namedPermitRequest(), nil })
			if response != nil {
				response.Body.Close()
			}
			if err == nil || sends != 1 || len(host.requests) != 1 || body.closed != 1 {
				t.Fatalf("malformed permit escaped validation: sends=%d callbacks=%d closes=%d err=%v", sends, len(host.requests), body.closed, err)
			}
		})
	}
}

func TestRequestPolicyPermitDecideGuardsAuthorityAndPreservesDiagnostics(t *testing.T) {
	for name, change := range map[string]func(*RequestRecoveryRequest){
		"no-evidence":       func(request *RequestRecoveryRequest) { request.Evidence = "" },
		"unknown-proof":     func(request *RequestRecoveryRequest) { request.Evidence = "idempotent" },
		"incomplete":        func(request *RequestRecoveryRequest) { request.Facts.Complete = false },
		"wrong-phase":       func(request *RequestRecoveryRequest) { request.Facts.Phase = "dispatch" },
		"no-operation":      func(request *RequestRecoveryRequest) { request.Facts.Operation = "" },
		"unknown-operation": func(request *RequestRecoveryRequest) { request.Facts.Operation = "arbitrary.call" },
		"invalid-step":      func(request *RequestRecoveryRequest) { request.Step = "wrong step" },
		"long-step":         func(request *RequestRecoveryRequest) { request.Step = strings.Repeat("a", 129) },
		"invalid-status":    func(request *RequestRecoveryRequest) { request.Facts.Status = 0 },
		"redirect":          func(request *RequestRecoveryRequest) { request.Facts.Status = 307 },
		"created":           func(request *RequestRecoveryRequest) { request.Facts.Status = 201 },
		"accepted":          func(request *RequestRecoveryRequest) { request.Facts.Status = 202 },
		"partial":           func(request *RequestRecoveryRequest) { request.Facts.Status = 206 },
	} {
		t.Run(name, func(t *testing.T) {
			request := namedPermitRequest()
			change(&request)
			host := &policyTestHost{decisions: []RequestRecoveryResult{namedPermitResult()}}
			result, err := DecideRequest(t.Context(), host, request)
			if err == nil || result.Retry {
				t.Fatalf("unsafe facts authorized recovery: %+v %v", result, err)
			}
			terminal := RequestRecoveryResult{Action: "terminal", Reason: "unsafe", Class: "unknown",
				Scope: "request", TotalRetries: 2, ProviderRetries: 1, NextAllowedAt: "2026-11-16T15:00:00Z"}
			host.decisions = []RequestRecoveryResult{terminal}
			result, err = DecideRequest(t.Context(), host, request)
			if err != nil || result != terminal {
				t.Fatalf("classification-only diagnostic was rejected or changed: %+v %v", result, err)
			}
		})
	}
	host := &policyTestHost{decisions: []RequestRecoveryResult{namedPermitResult()}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := DecideRequest(ctx, host, namedPermitRequest())
	if !errors.Is(err, context.Canceled) || result.Retry || len(host.requests) != 0 {
		t.Fatalf("canceled decision contacted the host or retained a permit: %+v %v", result, err)
	}
}

func TestRequestPolicyPermitResponseAndBodyOwnership(t *testing.T) {
	for _, status := range []int{301, 303, 307, 308} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			host := &policyTestHost{}
			body := &permitBody{Reader: strings.NewReader("redirect body")}
			request, _ := http.NewRequest(http.MethodGet, "https://fixture.invalid/read", nil)
			classified := 0
			response, decision, err := RequestWithPolicy(t.Context(), host, "named-read", request,
				policyRoundTrip(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, Body: body}, nil
				}), func(*http.Response) (RequestRecoveryRequest, error) {
					classified++
					return namedPermitRequest(), nil
				})
			if err != nil || decision != nil || response == nil || len(host.requests) != 0 || body.closed != 0 || classified != 1 {
				t.Fatalf("redirect lost caller ownership: decision=%+v err=%v", decision, err)
			}
			if response != nil {
				response.Body.Close()
			}
		})
	}
	for _, restore := range []bool{false, true} {
		name := "consumed"
		if restore {
			name = "restored"
		}
		t.Run(name, func(t *testing.T) {
			terminal := namedPermitResult()
			terminal.Retry, terminal.Action = false, "terminal"
			host := &policyTestHost{decisions: []RequestRecoveryResult{terminal}}
			body := &permitBody{Reader: strings.NewReader("original refusal")}
			request, _ := http.NewRequest(http.MethodGet, "https://fixture.invalid/read", nil)
			response, _, err := RequestWithPolicy(t.Context(), host, "named-read", request,
				policyRoundTrip(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 503, Body: body}, nil
				}), func(response *http.Response) (RequestRecoveryRequest, error) {
					data, err := io.ReadAll(response.Body)
					if restore {
						response.Body.Close()
						response.Body = io.NopCloser(strings.NewReader(string(data)))
					}
					return namedPermitRequest(), err
				})
			if restore {
				if err != nil || response == nil {
					t.Fatalf("restored body was rejected: %v", err)
				}
				data, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if string(data) != "original refusal" || body.closed != 1 {
					t.Fatalf("restored refusal bytes or ownership lost: %q closes=%d", data, body.closed)
				}
			} else {
				if response != nil {
					response.Body.Close()
				}
				if err == nil || len(host.requests) != 0 || body.closed != 1 {
					t.Fatalf("consumed response escaped the classifier: err=%v callbacks=%d closes=%d", err, len(host.requests), body.closed)
				}
			}
		})
	}
}

type permitHostCall func(context.Context, string, any, any) error

func (callback permitHostCall) HostCall(ctx context.Context, method string, request any, result any) error {
	return callback(ctx, method, request, result)
}

func TestRequestPolicyPermitCallbackErrorClearsAuthority(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		expected := errors.New("callback failed")
		if canceled {
			expected = context.Canceled
		}
		result, err := DecideRequest(ctx, permitHostCall(func(_ context.Context, _ string, _ any, output any) error {
			*output.(*RequestRecoveryResult) = namedPermitResult()
			if canceled {
				cancel()
				return nil
			}
			return expected
		}), namedPermitRequest())
		if !errors.Is(err, expected) || result.Retry {
			t.Fatalf("failed callback retained authority: %+v %v", result, err)
		}
	}
}

func TestRequestPolicyPermitInvalidResponseNeverRecovers(t *testing.T) {
	for _, forwarded := range []bool{false, true} {
		request, _ := http.NewRequest(http.MethodGet, "https://fixture.invalid/read", nil)
		host := &policyTestHost{decisions: []RequestRecoveryResult{namedPermitResult()}}
		body := &permitBody{Reader: strings.NewReader("response")}
		reply := &http.Response{StatusCode: 0, Body: body}
		if forwarded {
			reply.StatusCode = 503
			reply.Request, _ = http.NewRequest(http.MethodGet, "https://fixture.invalid/other", nil)
		}
		response, _, err := RequestWithPolicy(t.Context(), host, "named-read", request,
			policyRoundTrip(func(*http.Request) (*http.Response, error) { return reply, nil }),
			func(*http.Response) (RequestRecoveryRequest, error) {
				t.Fatal("invalid response reached classification")
				return namedPermitRequest(), nil
			})
		if err == nil || response != nil || len(host.requests) != 0 || body.closed != 1 {
			t.Fatalf("invalid response retained authority or ownership: %v closes=%d", err, body.closed)
		}
	}
}

func TestRequestPolicyPermitUnreplayableBody(t *testing.T) {
	t.Run("unreplayable", func(t *testing.T) {
		host := &policyTestHost{decisions: []RequestRecoveryResult{namedPermitResult()}}
		body := &permitBody{Reader: strings.NewReader("paid bytes")}
		request, _ := http.NewRequest(http.MethodPost, "https://fixture.invalid/read", body)
		sends := 0
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Errorf("unsafe permit panicked while replaying an unreplayable body: %v", recovered)
			}
		}()
		response, _, err := RequestWithPolicy(t.Context(), host, "named-read", request,
			policyRoundTrip(func(sent *http.Request) (*http.Response, error) {
				sends++
				io.Copy(io.Discard, sent.Body)
				return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("refused"))}, nil
			}), func(*http.Response) (RequestRecoveryRequest, error) { return namedPermitRequest(), nil })
		if response != nil {
			response.Body.Close()
		}
		if err == nil || sends != 1 || len(host.requests) != 1 || host.requests[0].Evidence != "" {
			t.Fatalf("unreplayable body gained authority: sends=%d callbacks=%d err=%v", sends, len(host.requests), err)
		}
	})
}

func TestRequestPolicyPermitTerminalContradictionsNeverSendAgain(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		reason string
		class  string
		scope  string
		valid  bool
	}{
		{"retry", "retry", "transient-rate", "account", true},
		{"future-reason", "future-host-diagnostic", "transient-outage", "request", true},
		{"optional-diagnostics", "", "", "", true},
		{"quota-wait", "retry", "quota", "model", true},
		{"unsafe", "unsafe", "transient-rate", "account", false},
		{"revoked", "revoked", "transient-rate", "account", false},
		{"canceled", "canceled", "transient-rate", "account", false},
		{"budget-exhausted", "budget-exhausted", "transient-rate", "account", false},
		{"auth-unavailable", "auth-recovery-unavailable", "transient-rate", "account", false},
		{"not-retryable", "not-retryable", "transient-rate", "account", false},
		{"attempt-limit", "attempt-limit", "transient-rate", "account", false},
		{"invalid-random", "invalid-random", "transient-rate", "account", false},
		{"delay-limit", "delay-limit", "transient-rate", "account", false},
		{"short-wait-exceeded", "short-wait-exceeded", "transient-rate", "account", false},
		{"selected-scope-refused", "selected-scope-refused", "transient-rate", "account", false},
		{"budget-unavailable", "budget-unavailable", "transient-rate", "account", false},
		{"policy-unavailable", "policy-unavailable", "transient-rate", "account", false},
		{"step-limit", "step-limit", "transient-rate", "account", false},
		{"no-eligible-account", "no-eligible-account", "transient-rate", "account", false},
		{"auth-class", "retry", "auth-refusal", "account", false},
		{"request-class", "retry", "request-refusal", "model", false},
		{"content-class", "retry", "content-refusal", "account", false},
		{"unknown-class", "retry", "unknown", "account", false},
		{"unknown-scope", "retry", "transient-rate", "unknown", false},
		{"request-quota", "retry", "quota", "request", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			failure := namedPermitRequest()
			grant := namedPermitResult()
			grant.Reason, grant.Class, grant.Scope = scenario.reason, scenario.class, scenario.scope
			host := &policyTestHost{decisions: []RequestRecoveryResult{grant}}
			decision, err := DecideRequest(t.Context(), host, failure)
			if scenario.valid {
				if err != nil || decision != grant {
					t.Fatalf("coherent permit rejected: %+v %v", decision, err)
				}
			} else if err == nil || decision.Retry {
				t.Errorf("terminal contradiction granted direct authority: %+v %v", decision, err)
			}
			terminal := grant
			terminal.Action, terminal.Retry = "terminal", false
			failure.Evidence = ""
			host.decisions = []RequestRecoveryResult{terminal}
			decision, err = DecideRequest(t.Context(), host, failure)
			if err != nil || decision != terminal {
				t.Fatalf("classification-only diagnostics changed: %+v %v", decision, err)
			}
			var sends atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil || string(body) != "same-request" || request.Method != http.MethodPost {
					t.Errorf("request changed: %q %v", body, err)
				}
				status := http.StatusOK
				if sends.Add(1) == 1 {
					status = http.StatusServiceUnavailable
				}
				writer.WriteHeader(status)
				writer.Write([]byte("bounded response"))
			}))
			defer server.Close()
			transport := &http.Transport{Proxy: nil}
			defer transport.CloseIdleConnections()
			request, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("same-request"))
			if err != nil {
				t.Fatal(err)
			}
			host.decisions = []RequestRecoveryResult{grant}
			response, _, err := RequestWithPolicy(t.Context(), host, failure.Step, request, transport,
				func(response *http.Response) (RequestRecoveryRequest, error) {
					if response.StatusCode == http.StatusOK {
						return RequestRecoveryRequest{}, nil
					}
					return namedPermitRequest(), nil
				})
			if response != nil {
				response.Body.Close()
			}
			wantSends := int32(1)
			if scenario.valid {
				wantSends = 2
			}
			if sends.Load() != wantSends || scenario.valid && err != nil || !scenario.valid && err == nil {
				t.Fatalf("terminal contradiction reached HTTP: sends=%d want=%d err=%v", sends.Load(), wantSends, err)
			}
		})
	}
}
