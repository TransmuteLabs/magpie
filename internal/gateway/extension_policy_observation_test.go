package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

type idleRefusalBody struct {
	io.ReadCloser
	canceled <-chan struct{}
}

func (body idleRefusalBody) Read(data []byte) (int, error) {
	<-body.canceled
	return body.ReadCloser.Read(data)
}

func TestExtensionIdleTimeoutStopsFinalRefusalFallback(t *testing.T) {
	var sends atomic.Int32
	id := installPolicyRouteFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if sends.Add(1) == 1 {
			json.NewEncoder(w).Encode(policyFixtureRefusal("quota", "account", nil))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{})
	})
	quietFast(t)
	previousQuiet := translatedQuietLongest
	t.Cleanup(func() { translatedQuietLongest = previousQuiet })
	translatedQuietLongest = 750 * time.Millisecond
	previousHook := provider.SendsThroughPlugin
	var held int
	provider.SendsThroughPlugin = func(selected string, req *http.Request) func(*http.Response, error) {
		done := previousHook(selected, req)
		return func(resp *http.Response, err error) {
			done(resp, err)
			if selected == id && err == nil && resp.StatusCode == http.StatusForbidden {
				held++
				resp.Body = idleRefusalBody{ReadCloser: resp.Body, canceled: req.Context().Done()}
			}
		}
	}
	t.Cleanup(func() { provider.SendsThroughPlugin = previousHook })
	observer := &fakeObserver{watch: true}
	t.Cleanup(usage.Observe(observer))
	server := New()
	t.Cleanup(server.idle.stop)
	reply := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/chat/completions",
		`{"model":"`+id+`/free","stream":true,"messages":[{"role":"user","content":"late refusal"}]}`)
	body := reply.Body.String()
	if held != 1 || sends.Load() != 1 || strings.Count(body, "sent nothing for") != 1 ||
		strings.Contains(body, "Upstream refused the selected request") {
		t.Fatalf("idle timeout resolved a late refusal: held=%d sends=%d status=%d body=%s", held, sends.Load(), reply.Code, body)
	}
	rows := usage.Load(time.Time{})
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(rows) != 1 || len(observer.attempts) != 1 || len(observer.ends) != 1 ||
		rows[0].Status != http.StatusGatewayTimeout || observer.attempts[0].Status != rows[0].Status ||
		observer.ends[0].Status != rows[0].Status {
		t.Fatalf("idle timeout disagrees with Watch or the ledger: rows=%+v attempts=%+v ends=%+v", rows, observer.attempts, observer.ends)
	}
}

type idleStreamTail struct {
	io.ReadCloser
	canceled <-chan struct{}
}

func (body idleStreamTail) Read(data []byte) (int, error) {
	count, err := body.ReadCloser.Read(data)
	if count == 0 && err == io.EOF {
		<-body.canceled
	}
	return count, err
}

func TestExtensionIdleTimeoutPreservesAcceptedOutcome(test *testing.T) {
	const chunk = "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"free\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"visible fixture output\"}}]}\n\n"
	for _, scenario := range []struct {
		name  string
		chunk string
	}{
		{name: "held"},
		{name: "passed", chunk: chunk},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			var sends atomic.Int32
			id := installPolicyRouteFixture(test, func(writer http.ResponseWriter, request *http.Request) {
				sends.Add(1)
				json.NewEncoder(writer).Encode(map[string]string{"chatStream": scenario.chunk})
			})
			quietFast(test)
			previousQuiet := translatedQuietLongest
			translatedQuietLongest = 750 * time.Millisecond
			test.Cleanup(func() { translatedQuietLongest = previousQuiet })
			previousHook := provider.SendsThroughPlugin
			provider.SendsThroughPlugin = func(selected string, req *http.Request) func(*http.Response, error) {
				done := previousHook(selected, req)
				return func(resp *http.Response, err error) {
					done(resp, err)
					if selected == id && err == nil && resp.StatusCode == http.StatusOK {
						resp.Body = idleStreamTail{ReadCloser: resp.Body, canceled: req.Context().Done()}
					}
				}
			}
			test.Cleanup(func() { provider.SendsThroughPlugin = previousHook })
			observer := &fakeObserver{watch: true}
			test.Cleanup(usage.Observe(observer))
			server := New()
			test.Cleanup(server.idle.stop)
			ctx, cancel := context.WithTimeout(test.Context(), 10*time.Second)
			defer cancel()
			request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(
				`{"model":"`+id+`/free","stream":true,"messages":[{"role":"user","content":"idle after acceptance"}]}`))
			request.RemoteAddr = "127.0.0.1:12345"
			request.Header.Set("Content-Type", "application/json")
			reply := httptest.NewRecorder()
			server.Front().ServeHTTP(reply, request)
			body := reply.Body.String()
			if sends.Load() != 1 || strings.Count(body, `"error":`) != 1 ||
				!strings.Contains(body, "extension operation accepted") ||
				!strings.Contains(body, "automatic replay stopped") ||
				strings.Contains(body, "sent nothing for") ||
				strings.Contains(body, "visible fixture output") != (scenario.chunk != "") {
				test.Fatalf("accepted idle stream lost its sole terminal outcome: sends=%d status=%d body=%s", sends.Load(), reply.Code, body)
			}
			rows := usage.Load(time.Time{})
			observer.mu.Lock()
			defer observer.mu.Unlock()
			if len(rows) != 1 || len(observer.attempts) != 1 || len(observer.ends) != 1 ||
				rows[0].Status != http.StatusConflict || observer.attempts[0].Status != rows[0].Status ||
				observer.ends[0].Status != rows[0].Status {
				test.Fatalf("accepted idle stream disagrees with Watch or the ledger: rows=%+v attempts=%+v ends=%+v", rows, observer.attempts, observer.ends)
			}
		})
	}
}

func TestExtensionFinalRefusalObservationSanitizesMachineFields(test *testing.T) {
	for _, scenario := range []struct {
		name, code, kind string
		want             usage.UpstreamFailure
	}{
		{"business status", "1008", "quota", usage.UpstreamFailure{Status: 200, Code: "1008", Type: "quota"}},
		{"identifier limits", strings.Repeat("c", 256), strings.Repeat("t", 128), usage.UpstreamFailure{Status: 200, Code: strings.Repeat("c", 256), Type: strings.Repeat("t", 128)}},
		{"private identifiers", "Bearer PRIVATE_SECRET", "quota\nPRIVATE_SECRET", usage.UpstreamFailure{Status: 200}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			var sends atomic.Int32
			id := installPolicyRouteFixture(test, func(writer http.ResponseWriter, request *http.Request) {
				sends.Add(1)
				json.NewEncoder(writer).Encode(map[string]any{
					"status": 200, "outcome": "not-started", "refusal": map[string]any{
						"step": "inference", "evidence": "rejected-before-effect",
						"facts": map[string]any{"status": 200, "code": scenario.code, "type": scenario.kind,
							"message": "PRIVATE_MESSAGE", "headers": map[string]string{"x-fixture": "PRIVATE_HEADER"},
							"operation": "provider.execute", "phase": "response", "complete": true},
						"rules": []any{map[string]any{"id": "fixture.host-rule", "status": []int{200}, "class": "quota", "scope": "request"}},
					},
				})
			})
			observer := &fakeObserver{watch: true, bodies: usage.WholeBodies}
			test.Cleanup(usage.Observe(observer))
			server := New()
			test.Cleanup(server.idle.stop)
			response := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/chat/completions",
				`{"model":"`+id+`/free","messages":[{"role":"user","content":"machine observation"}]}`)
			calls, rows := server.Recent(), usage.Load(time.Time{})
			if response.Code != 429 || sends.Load() != 1 || len(calls) != 1 || len(rows) != 1 {
				test.Fatalf("final refusal changed dispatch/accounting: status=%d sends=%d calls=%d rows=%d", response.Code, sends.Load(), len(calls), len(rows))
			}
			observer.mu.Lock()
			projections := append([]usage.Record{rows[0]}, observer.rows...)
			projections = append(projections, observer.attempts...)
			projections = append(projections, observer.ends...)
			observer.mu.Unlock()
			if calls[0].UpstreamFailure != scenario.want || len(projections) != 4 {
				test.Fatalf("ordinary refusal lost machine evidence: got=%+v want=%+v projections=%d", calls[0].UpstreamFailure, scenario.want, len(projections))
			}
			for _, row := range projections {
				if row.UpstreamFailure != scenario.want {
					test.Errorf("projection changed original evidence: got=%+v want=%+v", row.UpstreamFailure, scenario.want)
				}
				raw, _ := json.Marshal(row)
				if strings.Contains(string(raw), "PRIVATE_") {
					test.Error("observation persisted private refusal fields")
				}
			}
			raw, _ := json.Marshal(calls[0])
			if strings.Contains(string(raw)+response.Body.String(), "PRIVATE_") {
				test.Error("Recent/client exposed private refusal fields")
			}
		})
	}
}

func TestExtensionCachedPolicyRefusalRecordsRejectedRequest(test *testing.T) {
	var sends atomic.Int32
	id := installPolicyRouteFixture(test, func(writer http.ResponseWriter, request *http.Request) {
		sends.Add(1)
		json.NewEncoder(writer).Encode(policyFixtureRefusal("quota", "model", []any{
			map[string]any{"id": "free", "models": []string{"free"}, "resetAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)},
		}))
	})
	if err := extensionhost.Default().Store().DeleteAccount("fixture.policy", "vendor", "bob", 1); err != nil {
		test.Fatal(err)
	}
	provider.Changed()
	observer := &fakeObserver{watch: true, bodies: usage.WholeBodies}
	test.Cleanup(usage.Observe(observer))
	server := New()
	test.Cleanup(server.idle.stop)
	for range 2 {
		response := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/chat/completions",
			`{"model":"`+id+`/free","messages":[{"role":"user","content":"cached policy"}]}`)
		if response.Code != 403 {
			test.Fatalf("cached refusal changed routing status: %d", response.Code)
		}
	}
	calls, rows := server.Recent(), usage.Load(time.Time{})
	if len(calls) != 2 || len(rows) != 2 || sends.Load() != 1 {
		test.Fatalf("cached decision lost accounting or invented dispatch: calls=%d rows=%d sends=%d", len(calls), len(rows), sends.Load())
	}
	cached := calls[0]
	for _, row := range rows {
		if row.RouteID == cached.Route && (!row.Rejected || row.Status != 403 || row.UpstreamFailure != (usage.UpstreamFailure{})) {
			test.Fatalf("cached decision is not an honest rejected row: %+v", row)
		}
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.rows) != 2 || len(observer.ends) != 2 || len(observer.attempts) != 1 {
		test.Fatalf("cached decision invented/lost observer events: ledger=%d end=%d attempt=%d", len(observer.rows), len(observer.ends), len(observer.attempts))
	}
}

func TestExtensionPolicyFallbackObservationStaysWithItsTry(test *testing.T) {
	var sends atomic.Int32
	id := installPolicyRouteFixture(test, func(writer http.ResponseWriter, request *http.Request) {
		sends.Add(1)
		var selected struct{ Account string }
		if err := json.NewDecoder(request.Body).Decode(&selected); err != nil {
			test.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if selected.Account == "alice" {
			json.NewEncoder(writer).Encode(policyFixtureRefusal("quota", "account", nil))
			return
		}
		json.NewEncoder(writer).Encode(map[string]any{})
	})
	observer := &fakeObserver{watch: true}
	test.Cleanup(usage.Observe(observer))
	server := New()
	test.Cleanup(server.idle.stop)
	response := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/chat/completions",
		`{"model":"`+id+`/free","messages":[{"role":"user","content":"eligible sibling"}]}`)
	calls, rows := server.Recent(), usage.Load(time.Time{})
	if response.Code != 200 || sends.Load() != 2 || len(calls) != 1 || len(rows) != 1 {
		test.Fatalf("sibling boundary changed: status=%d sends=%d calls=%d rows=%d", response.Code, sends.Load(), len(calls), len(rows))
	}
	if calls[0].UpstreamFailure != (usage.UpstreamFailure{}) || rows[0].UpstreamFailure != (usage.UpstreamFailure{}) {
		test.Fatal("successful sibling inherited the previous refusal")
	}
	server.trace.mu.Lock()
	defer server.trace.mu.Unlock()
	if len(server.trace.routes) != 1 || len(server.trace.routes[0].Tries) != 2 {
		test.Fatal("sibling route lost its actual tries")
	}
	tries := server.trace.routes[0].Tries
	if tries[0].UpstreamFailure != (usage.UpstreamFailure{Status: 403, Code: "denied"}) || tries[1].UpstreamFailure != (usage.UpstreamFailure{}) {
		test.Fatalf("refusal evidence left its actual try: %+v", tries)
	}
}
