package extension

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type policyTestHost struct {
	decisions []RequestRecoveryResult
	requests  []RequestRecoveryRequest
}

func (host *policyTestHost) HostCall(_ context.Context, method string, params any, out any) error {
	if method != MethodRequestRecover {
		return errors.New("unexpected host method")
	}
	host.requests = append(host.requests, params.(RequestRecoveryRequest))
	if len(host.decisions) == 0 {
		return errors.New("no fixture decision")
	}
	*out.(*RequestRecoveryResult) = host.decisions[0]
	host.decisions = host.decisions[1:]
	return nil
}

type policyRoundTrip func(*http.Request) (*http.Response, error)

func (transport policyRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestRequestPolicyDriverPreservesRequestAndUsesOnlyPermits(t *testing.T) {
	host := &policyTestHost{decisions: []RequestRecoveryResult{{Step: "generate", Action: "wait", Retry: true}, {Retry: false, Class: "quota", Scope: "account", Rule: "quota"}}}
	request, _ := http.NewRequest(http.MethodPost, "https://fixture.invalid/generate", strings.NewReader("same business bytes"))
	requests := 0
	transport := policyRoundTrip(func(sent *http.Request) (*http.Response, error) {
		requests++
		body, _ := io.ReadAll(sent.Body)
		if string(body) != "same business bytes" || sent.URL.String() != request.URL.String() || sent.Method != request.Method {
			t.Fatal("request changed")
		}
		return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader("refused"))}, nil
	})
	response, decision, err := RequestWithPolicy(context.Background(), host, "generate", request, transport, func(response *http.Response) (RequestRecoveryRequest, error) {
		return RequestRecoveryRequest{Facts: RequestFailureFacts{Operation: MethodProviderExecute, Phase: "response", Complete: true}, Evidence: "rejected-before-effect"}, nil
	})
	if err != nil || requests != 2 || len(host.requests) != 2 || decision == nil || decision.Class != "quota" {
		t.Fatalf("calls=%d decision=%+v err=%v", requests, decision, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "refused" {
		t.Fatal("refusal body lost")
	}
}

func TestRequestPolicyDriverClassifiesWithoutEvidenceAndDoesNotRetryTransportFailure(t *testing.T) {
	host := &policyTestHost{decisions: []RequestRecoveryResult{{Class: "request-refusal", Scope: "request", Rule: "request"}}}
	request, _ := http.NewRequest(http.MethodGet, "https://fixture.invalid/search", nil)
	classify := func(*http.Response) (RequestRecoveryRequest, error) {
		return RequestRecoveryRequest{Facts: RequestFailureFacts{Operation: MethodModuleCall, Phase: "response", Complete: true}}, nil
	}
	response, decision, err := RequestWithPolicy(context.Background(), host, "search", request, policyRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("bad request"))}, nil
	}), classify)
	if err != nil || decision == nil || decision.Class != "request-refusal" || len(host.requests) != 1 || host.requests[0].Evidence != "" {
		t.Fatalf("%+v %v", decision, err)
	}
	response.Body.Close()
	disconnected := errors.New("connection lost")
	_, _, err = RequestWithPolicy(context.Background(), host, "search", request, policyRoundTrip(func(*http.Request) (*http.Response, error) { return nil, disconnected }), classify)
	if !errors.Is(err, disconnected) || len(host.requests) != 1 {
		t.Fatal("transport error attempted recovery", err)
	}
}
