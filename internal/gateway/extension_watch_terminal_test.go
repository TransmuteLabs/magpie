package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/otlp"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const extensionWatchReply = `{"id":"fixture","model":"m1","provider":"fixture-upstream","service_tier":"priority","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":78,"completion_tokens":13,"credit":0.25}}`

const extensionWatchWorker = `package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func main() {
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		panic(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		panic(err)
	}
	module := sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{
		sdk.MethodProviderExecute: func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
			var request sdk.ExecuteRequest
			if err := json.Unmarshal(call.Params, &request); err != nil {
				return nil, err
			}
			var body struct {
				Messages []struct { Content string }
			}
			if err := json.Unmarshal(request.Body, &body); err != nil || len(body.Messages) != 1 {
				return nil, fmt.Errorf("invalid fixture conversation: %v", err)
			}
			mode := body.Messages[0].Content
			if mode == "not-started" {
				return nil, &sdk.Error{Code: "not_started", Message: "fixture did not start", Outcome: "not-started"}
			}
			if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
				return nil, err
			}
			totals := &sdk.Usage{InputTokens: 41, OutputTokens: 3, CachedTokens: 5, CacheWriteTokens: 2}
			if mode == "rich" || mode == "rich accepted" {
				count := func(value int64) *int64 { return &value }
				text := func(value string) *string { return &value }
				credits := 0.25
				totals = &sdk.Usage{Facts: &sdk.ExecutionUsageFacts{
					ExecutionCounts: sdk.ExecutionCounts{InputTokens: count(17), OutputTokens: count(5),
						CachedTokens: count(4), CacheWriteTokens: count(7), CacheWrite1hTokens: count(3), ReasoningTokens: count(2)},
					Deferred: &sdk.ExecutionCounts{InputTokens: count(11), OutputTokens: count(1)},
					ServedModel: text("served-fixture"), UpstreamProvider: text("fixture-upstream"),
					Tier: text("flex"), RequestID: text("fixture-request"), ResponseID: text("fixture-response"),
					Credits: &credits, Cost: &sdk.ExecutionCost{Amount: 0.002, Currency: "USD"},
				}}
			}
			for range 2 {
				if err := stream.Emit(ctx, "usage", totals); err != nil {
					return nil, err
				}
			}
			if mode == "accepted" || mode == "uncertain" {
				return nil, &sdk.Error{Code: "fixture_failed", Message: "fixture stopped", Outcome: mode}
			}
			if mode == "rich accepted" {
				return nil, &sdk.Error{Code: "fixture_failed", Message: "fixture stopped", Outcome: "accepted"}
			}
			if mode == "quiet held" {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			contentType, reply := "application/json", ` + "`" + extensionWatchReply + "`" + `
			switch mode {
			case "quiet passing":
				contentType = "text/event-stream"
				reply = "data: {\"id\":\"quiet\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial reply\"}}]}\n\n"
			case "empty whole":
				reply = "{\"id\":\"empty\",\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":\"stop\"}]}"
			case "empty stream", "void stream":
				contentType = "text/event-stream"
				reply = "data: {\"id\":\"empty\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"
				if mode == "empty stream" {
					reply += "data: {\"id\":\"empty\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
				} else {
					reply += "data: {\"id\":\"empty\",\"choices\":[],\"usage\":{\"prompt_tokens\":41,\"completion_tokens\":3,\"total_tokens\":44}}\n\n"
				}
				reply += "data: [DONE]\n\n"
			}
			if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: http.StatusOK,
				Headers: http.Header{"Content-Type": {contentType}}}); err != nil {
				return nil, err
			}
			if err := stream.Emit(ctx, "data", []byte(reply)); err != nil {
				return nil, err
			}
			switch mode {
			case "read failure":
				return nil, &sdk.Error{Code: "response_cut", Message: "fixture response read failed", Outcome: "uncertain"}
			case "close failure", "quiet passing":
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return sdk.ExecuteResult{Status: http.StatusOK, Outcome: "completed", Usage: totals}, nil
		},
	}}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`

type extensionWatchBody struct {
	io.ReadCloser
	reader        io.Reader
	readFailures  int
	closes        int
	closeFailures int
	failClose     bool
}

func (body *extensionWatchBody) Read(data []byte) (int, error) {
	count, err := body.reader.Read(data)
	if err != nil && err != io.EOF {
		body.readFailures++
	}
	return count, err
}

func (body *extensionWatchBody) Close() error {
	body.closes++
	err := body.ReadCloser.Close()
	if body.failClose {
		body.closeFailures++
		return io.ErrClosedPipe
	}
	return err
}

type extensionWatchSpan struct {
	TraceID  string `json:"traceId"`
	SpanID   string `json:"spanId"`
	ParentID string `json:"parentSpanId"`
	Kind     int    `json:"kind"`
	Attrs    []struct {
		Key   string
		Value struct {
			Integer string `json:"intValue"`
		}
	} `json:"attributes"`
}

func (span extensionWatchSpan) integer(key string) int {
	for _, attr := range span.Attrs {
		if attr.Key == key {
			value, _ := strconv.Atoi(attr.Value.Integer)
			return value
		}
	}
	return -1
}

func TestExtensionTerminalAttemptsReachWatchAndLedger(t *testing.T) {
	directory := t.TempDir()
	manifest := extensionGatewayManifest()
	manifest.ID = "fixture.watch"
	source := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(source, []byte(extensionWatchWorker), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	build := proc.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly",
		"-o", filepath.Join(directory, manifest.Entry), source)
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build SDK watch fixture: %v\n%s", err, output)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"accepted", "uncertain", "read failure", "close failure", "completed", "not-started", "rich", "rich accepted", "empty whole", "empty stream", "void stream", "quiet held", "quiet passing"} {
		t.Run(mode, func(t *testing.T) {
			fresh(t)
			quiet := strings.HasPrefix(mode, "quiet ")
			if quiet {
				quietFast(t)
				previous := translatedQuietLongest
				t.Cleanup(func() { translatedQuietLongest = previous })
				translatedQuietLongest = 750 * time.Millisecond
			}
			t.Setenv("MAGPIE_ADDR", "")
			t.Setenv("MAGPIE_BUN", filepath.Join(t.TempDir(), "absent"))
			manager := extensionhost.Default()
			t.Cleanup(func() { closeSDKGatewayHook(t, manager) })
			if _, err := manager.Install(t.Context(), directory); err != nil {
				t.Fatal(err)
			}
			provider.Changed()
			id := provider.ExtensionProviderID(manifest.ID, manifest.Providers[0].ID)
			if err := provider.Save(provider.Provider{ID: "fallback", Name: "Fallback",
				Chat: "http://fallback.invalid/v1", Key: "fixture", Models: []string{"m1"}}); err != nil {
				t.Fatal(err)
			}
			if err := provider.SaveGroup(provider.Group{ID: "watched", Name: "Watched", Routing: provider.Ordered,
				Members: []string{id + "/m1", "fallback/m1"}}); err != nil {
				t.Fatal(err)
			}
			var attempts, fallbacks int
			var observedBody *extensionWatchBody
			previous := provider.SendsThroughPlugin
			provider.SendsThroughPlugin = func(selected string, request *http.Request) func(*http.Response, error) {
				done := previous(selected, request)
				if selected == id {
					attempts++
				}
				return func(response *http.Response, err error) {
					done(response, err)
					if selected == id && err == nil {
						observedBody = &extensionWatchBody{ReadCloser: response.Body, reader: response.Body, failClose: mode == "close failure"}
						if observedBody.failClose {
							observedBody.reader = io.LimitReader(response.Body, int64(len(extensionWatchReply)))
						}
						response.Body = observedBody
					}
				}
			}
			t.Cleanup(func() { provider.SendsThroughPlugin = previous })
			observer := &fakeObserver{watch: true, bodies: usage.CapturedBodies}
			t.Cleanup(usage.Observe(observer))
			var lock sync.Mutex
			var spans []extensionWatchSpan
			collector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != "/v1/traces" {
					http.NotFound(writer, request)
					return
				}
				var envelope struct {
					Resources []struct {
						Scopes []struct {
							Spans []extensionWatchSpan
						} `json:"scopeSpans"`
					} `json:"resourceSpans"`
				}
				if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
					t.Errorf("OTLP collector: %v", err)
					writer.WriteHeader(400)
					return
				}
				lock.Lock()
				for _, resource := range envelope.Resources {
					for _, scope := range resource.Scopes {
						spans = append(spans, scope.Spans...)
					}
				}
				lock.Unlock()
				io.WriteString(writer, `{}`)
			}))
			t.Cleanup(collector.Close)
			for name, value := range map[string]string{
				"MAGPIE_OTEL_ENABLED": "true", "MAGPIE_OTEL_ENDPOINT": collector.URL,
				"MAGPIE_OTEL_HEADERS": "", "MAGPIE_OTEL_METRICS": "false",
				"MAGPIE_OTEL_BODIES": "false", "MAGPIE_OTEL_SESSIONS": "false",
			} {
				t.Setenv(name, value)
			}
			exporter := otlp.New()
			stop := exporter.Serve()
			t.Cleanup(stop)
			t.Cleanup(usage.Observe(exporter))
			server := New()
			t.Cleanup(server.idle.stop)
			server.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				fallbacks++
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Request: request,
					Body: io.NopCloser(strings.NewReader(`{"id":"fallback","choices":[{"message":{"role":"assistant","content":"fallback"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2}}`))}, nil
			})}
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"group/watched","stream":`+strconv.FormatBool(strings.HasSuffix(mode, " stream") || quiet)+`,"messages":[{"role":"user","content":"`+mode+`"}]}`))
			request.RemoteAddr = "127.0.0.1:12345"
			reply := httptest.NewRecorder()
			server.Front().ServeHTTP(reply, request)
			stop()
			rich := mode == "rich" || mode == "rich accepted"
			terminal := mode != "completed" && mode != "not-started" && mode != "rich"
			status, wantFallbacks, wantAttempts, input, output := 200, 0, 1, 41, 3
			if rich {
				input, output = 28, 6
			}
			if terminal {
				status = http.StatusConflict
			}
			if mode == "not-started" {
				wantFallbacks, wantAttempts, input, output = 1, 2, 11, 2
			}
			wireStatus := status
			if quiet {
				wireStatus = http.StatusOK
				body := reply.Body.String()
				if strings.Count(body, `"error":`) != 1 || strings.Count(body, "automatic replay stopped") != 1 ||
					strings.Contains(body, "partial reply") != (mode == "quiet passing") {
					t.Errorf("idle SDK stream did not end with exactly one operation outcome: %s", body)
				}
			}
			if reply.Code != wireStatus || attempts != 1 || fallbacks != wantFallbacks {
				t.Fatalf("SDK outcome replayed or changed: status=%d SDK=%d fallback=%d body=%s", reply.Code, attempts, fallbacks, reply.Body)
			}
			headers := reply.Result().Header
			if terminal && !quiet && (headers.Get("X-Should-Retry") != "false" || headers.Get("X-Magpie-Operation") == "") {
				t.Errorf("terminal SDK outcome lost replay fence: %v", headers)
			}
			if observedBody != nil && observedBody.closes != 1 {
				t.Errorf("SDK response closed %d times", observedBody.closes)
			}
			if mode == "read failure" && (observedBody == nil || observedBody.readFailures != 1) {
				t.Error("fixture did not exercise an SDK response read failure")
			}
			if mode == "close failure" && (observedBody == nil || observedBody.closeFailures != 1) {
				t.Error("fixture did not exercise an SDK response close failure")
			}
			observer.mu.Lock()
			defer observer.mu.Unlock()
			if len(observer.begun) != 1 || len(observer.attempts) != wantAttempts || len(observer.ends) != 1 {
				t.Errorf("terminal SDK attempt disappeared: Begin=%d Attempt=%d End=%d, want 1/%d/1",
					len(observer.begun), len(observer.attempts), len(observer.ends), wantAttempts)
			}
			if len(observer.rows) != 1 || !observer.rows[0].Watched || observer.rows[0].HandedOn {
				t.Fatalf("SDK billing was duplicated or lost its watch: %+v", observer.rows)
			}
			row := observer.rows[0]
			if quiet {
				_, tail, found := strings.Cut(reply.Body.String(), "inspect the operation before retrying (")
				operation, _, ended := strings.Cut(tail, ")")
				if !found || !ended || operation == "" {
					t.Errorf("idle stream lost its operation ID: %s", reply.Body)
				} else if _, err := manager.Store().Operation(manifest.ID, operation); err != nil {
					t.Errorf("idle stream named an unknown operation %q: %v", operation, err)
				}
			}
			records := usage.Load(time.Time{})
			if len(records) != 1 || row.Status != status || row.Input != input || row.Output != output ||
				records[0].Input != row.Input || records[0].Output != row.Output || records[0].Status != status {
				t.Errorf("SDK billing differs from its snapshot: observer=%+v ledger=%+v", row, records)
			}
			cache := 0
			if mode != "not-started" {
				cache = 7
				if rich {
					cache = 11
				}
				if !rich && (row.CacheRead != 5 || row.CacheWrite != 2) {
					t.Errorf("SDK usage events were added instead of replaced: %+v", row)
				}
			}
			if len(observer.attempts) == wantAttempts {
				attempt := observer.attempts[wantAttempts-1]
				if attempt.RouteID != row.RouteID || attempt.Provider != row.Provider || attempt.Model != row.Model ||
					attempt.Input != input || attempt.Output != output || attempt.CacheRead+attempt.CacheWrite != cache || attempt.Status != status {
					t.Errorf("Watch.Attempt lost terminal usage or identity: attempt=%+v ledger=%+v", attempt, row)
				}
				if mode == "read failure" || mode == "close failure" || mode == "completed" {
					if attempt.Upstream != row.Upstream || attempt.TierServed != row.TierServed ||
						attempt.Credits == nil || row.Credits == nil || *attempt.Credits != *row.Credits {
						t.Errorf("Watch.Attempt dropped measured reply metadata: attempt=%+v ledger=%+v", attempt, row)
					}
				}
				if rich && len(records) == 1 {
					for _, measured := range []usage.Record{attempt, row, records[0]} {
						if measured.CacheRead != 4 || measured.CacheWrite != 7 || measured.CacheWrite1h != 3 ||
							measured.Reasoning != 2 || measured.Served != "served-fixture" || measured.Upstream != "fixture-upstream" ||
							measured.RequestID != "fixture-request" || measured.UpstreamResponseID != "fixture-response" ||
							measured.TierServed != "flex" || measured.Credits == nil || *measured.Credits != 0.25 ||
							measured.ReportedCost == nil || measured.ReportedCost.Currency != "USD" ||
							measured.OwnCost == nil || *measured.OwnCost != 0.002 {
							t.Errorf("chat accounting lost execution facts: status=%d cache=%d/%d/%d reasoning=%d model=%q upstream=%q request=%q upstreamResponse=%q tier=%q credits=%v cost=%v usd=%v",
								measured.Status, measured.CacheRead, measured.CacheWrite, measured.CacheWrite1h, measured.Reasoning,
								measured.Served, measured.Upstream, measured.RequestID, measured.UpstreamResponseID,
								measured.TierServed, measured.Credits, measured.ReportedCost, measured.OwnCost)
						}
					}
				}
			}
			if rich {
				wantClientID := ""
				if mode == "rich" {
					wantClientID = clientResponseID(t, provider.Chat, reply.Body.String(), false)
				}
				if row.ResponseID != wantClientID || len(records) == 1 && records[0].ResponseID != wantClientID {
					t.Errorf("vendor accounting replaced the delivered response ID: observer=%q want=%q", row.ResponseID, wantClientID)
				}
			}
			if len(observer.ends) == 1 {
				end := observer.ends[0]
				if end.RouteID != row.RouteID || end.Status != status || end.Input+end.Output+end.CacheRead+end.CacheWrite != 0 {
					t.Errorf("Watch.End duplicated billing or changed the result: %+v", end)
				}
			}
			lock.Lock()
			defer lock.Unlock()
			var root extensionWatchSpan
			var children []extensionWatchSpan
			for _, span := range spans {
				if span.Kind == 2 {
					root = span
				} else {
					children = append(children, span)
				}
			}
			if len(spans) != wantAttempts+1 || len(children) != wantAttempts || root.SpanID == "" {
				t.Errorf("OTLP lost or duplicated terminal attempts: spans=%+v", spans)
			}
			var billedInput, billedOutput int
			for _, span := range children {
				if span.ParentID != root.SpanID || span.TraceID != root.TraceID {
					t.Errorf("OTLP attempt lost its request: root=%+v child=%+v", root, span)
				}
				billedInput += span.integer("gen_ai.usage.input_tokens")
				billedOutput += span.integer("gen_ai.usage.output_tokens")
			}
			if billedInput != input+cache || billedOutput != output {
				t.Errorf("OTLP terminal billing=%d/%d, want %d/%d", billedInput, billedOutput, input+cache, output)
			}
		})
	}
}

func TestObserverAttemptKeepsCacheWriteLifetime(t *testing.T) {
	fresh(t)
	setup(t, provider.Anthropic, &fake{ctype: "application/json",
		reply: `{"id":"m","type":"message","role":"assistant","model":"m1","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":17,"output_tokens":3,"cache_read_input_tokens":5,"cache_creation_input_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":3}}}`})
	observer := &fakeObserver{watch: true}
	t.Cleanup(usage.Observe(observer))
	server := New()
	t.Cleanup(server.idle.stop)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"fake/m1","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`))
	request.RemoteAddr = "127.0.0.1:12345"
	reply := httptest.NewRecorder()
	server.Front().ServeHTTP(reply, request)
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if reply.Code != 200 || len(observer.attempts) != 1 || len(observer.rows) != 1 {
		t.Fatalf("ordinary watched reply: status=%d attempts=%+v rows=%+v", reply.Code, observer.attempts, observer.rows)
	}
	attempt, row := observer.attempts[0], observer.rows[0]
	if attempt.CacheWrite != 7 || attempt.CacheWrite1h != 3 || row.CacheWrite1h != 3 {
		t.Fatalf("Watch.Attempt dropped the billed cache lifetime: attempt=%+v ledger=%+v", attempt, row)
	}
}
