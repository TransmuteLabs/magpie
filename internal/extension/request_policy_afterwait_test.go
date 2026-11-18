package extension

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/requestpolicy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func fixtureRequestPolicyAfterWait(ctx context.Context, call sdk.Call, stream *sdk.Stream, raw json.RawMessage) (any, error) {
	var input struct {
		URL      string `json:"url"`
		Evidence string `json:"evidence"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, err
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	send := func(method, path string) error {
		request, err := http.NewRequestWithContext(ctx, method, input.URL+path, nil)
		if err != nil {
			return err
		}
		response, err := transport.RoundTrip(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		_, err = io.Copy(io.Discard, response.Body)
		return err
	}
	acceptance := make(chan error, 1)
	go func() {
		err := send(http.MethodGet, "/control")
		if err == nil {
			err = stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation})
		}
		if err == nil {
			err = send(http.MethodPost, "/paid")
		}
		acceptance <- err
	}()
	method := http.MethodPost
	if input.Evidence == "read-only" {
		method = http.MethodGet
	}
	request, err := http.NewRequestWithContext(ctx, method, input.URL+"/step", strings.NewReader("unchanged request"))
	if err != nil {
		<-acceptance
		return nil, err
	}
	response, decision, err := sdk.RequestWithPolicy(ctx, stream, "wait-step", request, transport,
		func(response *http.Response) (sdk.RequestRecoveryRequest, error) {
			if response.StatusCode == http.StatusOK {
				return sdk.RequestRecoveryRequest{}, nil
			}
			return sdk.RequestRecoveryRequest{
				Evidence: input.Evidence,
				Facts: sdk.RequestFailureFacts{Status: response.StatusCode, Operation: call.Method, Phase: "response",
					Complete: true, Headers: map[string]string{"Retry-After": response.Header.Get("Retry-After")}},
				Rules: []sdk.RequestFailureRule{{ID: "pace", Status: []int{429}, Class: "transient-rate", Scope: "account"}},
			}, nil
		})
	acceptedErr := <-acceptance
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if acceptedErr != nil {
		return nil, acceptedErr
	}
	_, err = io.Copy(io.Discard, response.Body)
	return struct {
		Status   int                        `json:"status"`
		Decision *sdk.RequestRecoveryResult `json:"decision"`
	}{response.StatusCode, decision}, err
}

type requestPolicyWaitContext struct {
	context.Context
	budget  *requestpolicy.Budget
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (current *requestPolicyWaitContext) Done() <-chan struct{} {
	if used, _ := current.budget.Counts(""); used > 0 {
		current.once.Do(func() {
			close(current.entered)
			select {
			case <-current.release:
			case <-current.Context.Done():
			}
		})
	}
	return current.Context.Done()
}

func TestRequestPolicyAcceptanceDuringWait(t *testing.T) {
	for _, evidence := range []string{"rejected-before-effect", "read-only"} {
		t.Run(evidence, func(t *testing.T) {
			manager, scope := requestPolicyManager(t)
			zero, one, maximum := 0, 1, 5000
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			ctx = requestpolicy.WithConfig(ctx, &requestpolicy.Config{Defaults: requestpolicy.Options{
				BaseDelayMS: &zero, MaxRetries: &one, MaxTotalRetries: &one,
				MaxDelayMS: &maximum, ShortWaitMS: &maximum,
			}})
			budget := requestpolicy.FromContext(ctx)
			lease, err := manager.Acquire(ctx, scope.Plugin)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			waiting, releaseWait, releaseControl, paid := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
			var waitOnce, controlOnce sync.Once
			unblockWait := func() { waitOnce.Do(func() { close(releaseWait) }) }
			unblockControl := func() { controlOnce.Do(func() { close(releaseControl) }) }
			original := lease.client.hostCall
			lease.client.hostCall = func(callback context.Context, selected sdk.Scope, method string, raw json.RawMessage) (any, error) {
				if method == sdk.MethodRequestRecover {
					callback = &requestPolicyWaitContext{Context: callback, budget: budget, entered: waiting, release: releaseWait}
				}
				return original(callback, selected, method, raw)
			}
			var countsMu sync.Mutex
			counts := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				countsMu.Lock()
				counts[request.URL.Path]++
				number := counts[request.URL.Path]
				countsMu.Unlock()
				switch request.URL.Path {
				case "/control":
					select {
					case <-releaseControl:
					case <-request.Context().Done():
					}
				case "/paid":
					select {
					case paid <- struct{}{}:
					default:
					}
				case "/step":
					if number == 1 {
						writer.Header().Set("Retry-After", "0.1")
						writer.WriteHeader(http.StatusTooManyRequests)
					}
				}
				writer.Write([]byte("fixture response"))
			}))
			defer server.Close()
			value, _ := json.Marshal(map[string]string{"url": server.URL, "evidence": evidence})
			arguments, _ := json.Marshal(map[string]any{"action": "request-policy-afterwait", "value": json.RawMessage(value)})
			type completion struct {
				raw json.RawMessage
				err error
			}
			done := make(chan completion, 1)
			go func() {
				raw, err := lease.Call(ctx, sdk.MethodToolCall, scope, sdk.ToolRequest{ID: "tool", Arguments: arguments}, nil)
				done <- completion{raw, err}
			}()
			finished := false
			defer func() {
				cancel()
				unblockControl()
				unblockWait()
				if !finished {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("selected invocation did not stop")
					}
				}
			}()
			select {
			case <-waiting:
			case <-ctx.Done():
				t.Fatal("host did not enter its admitted wait")
			}
			unblockControl()
			select {
			case <-paid:
			case <-ctx.Done():
				t.Fatal("worker did not accept its paid operation during the host wait")
			}
			receipt, err := manager.Store().Operation(scope.Plugin, scope.Operation)
			if err != nil || receipt.State != "accepted" || receipt.Scope != scope {
				t.Fatalf("acceptance was not durable before the wait ended: %+v %v", receipt, err)
			}
			unblockWait()
			var result completion
			select {
			case result = <-done:
				finished = true
			case <-ctx.Done():
				t.Fatal("selected invocation did not finish")
			}
			if result.err != nil {
				t.Fatal(result.err)
			}
			var response struct {
				Status   int                        `json:"status"`
				Decision *sdk.RequestRecoveryResult `json:"decision"`
			}
			if err := json.Unmarshal(result.raw, &response); err != nil {
				t.Fatal(err)
			}
			wantSends, wantStatus := 1, http.StatusTooManyRequests
			if evidence == "read-only" {
				wantSends, wantStatus = 2, http.StatusOK
			}
			countsMu.Lock()
			sends, paidSends := counts["/step"], counts["/paid"]
			countsMu.Unlock()
			used, _ := budget.Counts("")
			if sends != wantSends || paidSends != 1 || used != 1 || response.Status != wantStatus {
				t.Fatalf("acceptance during wait lost replay/budget isolation: sends=%d paid=%d used=%d result=%+v",
					sends, paidSends, used, response)
			}
			if evidence == "rejected-before-effect" && (response.Decision == nil || response.Decision.Retry || response.Decision.Reason != "unsafe") {
				t.Fatalf("accepted business step obtained a repeat permit: %+v", response.Decision)
			}
		})
	}
}
