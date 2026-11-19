package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

const canonicalExtensionWorker = `package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func main() {
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		os.Exit(1)
	}
	var manifest sdk.Manifest
	if json.Unmarshal(raw, &manifest) != nil {
		os.Exit(1)
	}
	module := sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{
		sdk.MethodProviderExecute: func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
			var request sdk.ExecuteRequest
			if err := json.Unmarshal(call.Params, &request); err != nil {
				return nil, err
			}
			captured, err := json.Marshal(map[string]any{"request": request, "scope": call.Scope})
			if err != nil {
				return nil, err
			}
			response := map[string]any{
				"id": "canonical-fixture", "model": request.Model,
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{
					"role": "assistant", "content": string(captured)}, "finish_reason": "stop"}},
				"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1},
			}
			var options sdk.ExecuteOptions
			if err := json.Unmarshal(request.Options, &options); err != nil {
				return nil, err
			}
			if options.Path == "/v1/messages/count_tokens" {
				response = map[string]any{"input_tokens": 42, "captured": json.RawMessage(captured)}
			} else if request.Protocol == "anthropic" {
				response = map[string]any{
					"id": "canonical-fixture", "type": "message", "role": "assistant", "model": request.Model,
					"content": []any{map[string]any{"type": "text", "text": string(captured)}},
					"stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1},
				}
			}
			body, err := json.Marshal(response)
			if err != nil {
				return nil, err
			}
			headers := sdk.HeadersEvent{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}}
			switch request.Model {
			case "wire-variant":
				headers.SentModel = "vendor/wire-variant-high"
			case "wire-matching":
				headers.SentModel = request.Model
			case "wire-invalid":
				headers.SentModel = "invalid variant"
			}
			if request.Model == "wire-spoof" || request.Model == "wire-variant" {
				headers.Headers.Set("X-Magpie-Sent-Model", "vendor-untrusted")
				headers.Headers["x-magpie-sent-model"] = []string{"vendor-untrusted-lowercase"}
			}
			if err := stream.Emit(ctx, "headers", headers); err != nil {
				return nil, err
			}
			if err := stream.Emit(ctx, "data", body); err != nil {
				return nil, err
			}
			return sdk.ExecuteResult{Status: 200, Outcome: "completed"}, nil
		},
	}}
	if sdk.Serve(context.Background(), os.Stdin, os.Stdout, module) != nil {
		os.Exit(1)
	}
}
`

type canonicalExecution struct {
	Request sdk.ExecuteRequest `json:"request"`
	Scope   sdk.Scope          `json:"scope"`
}

func installCanonicalExtensionFixture(t *testing.T, modelIDs ...string) string {
	t.Helper()
	fresh(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	t.Setenv("MAGPIE_ADDR", "")
	directory := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	source := filepath.Join(t.TempDir(), "worker.go")
	if err := os.WriteFile(source, []byte(canonicalExtensionWorker), 0o600); err != nil {
		t.Fatal(err)
	}
	buildContext, stopBuild := context.WithTimeout(t.Context(), time.Minute)
	defer stopBuild()
	build := exec.CommandContext(buildContext, filepath.Join(runtime.GOROOT(), "bin", "go"), "build",
		"-mod=readonly", "-o", filepath.Join(directory, entry), source)
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build canonical fixture: %v\n%s", err, output)
	}
	if len(modelIDs) == 0 {
		modelIDs = []string{"m1"}
	}
	var models []sdk.Model
	for _, model := range modelIDs {
		models = append(models, sdk.Model{ID: model, Protocols: []protocolext.Protocol{"chat", "messages"},
			ContextWindow: 32768, MaxOutput: 4096})
	}
	manifest := sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: "fixture.canonical", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 3}, Capabilities: []sdk.Capability{sdk.OutboundExecutor}},
		Providers: []sdk.ProviderDescriptor{{ID: "canonical", Name: "Canonical fixture",
			Protocols: []protocolext.Protocol{"chat", "messages"},
			Models:    models}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := extensionhost.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Errorf("canonical fixture cleanup: %v", err)
		}
	})
	installContext, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if _, err := manager.Install(installContext, directory); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	return provider.ExtensionProviderID(manifest.ID, manifest.Providers[0].ID)
}

func readCanonicalExecution(t *testing.T, response *httptest.ResponseRecorder) canonicalExecution {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("canonical execution failed: %d %s", response.Code, response.Body)
	}
	var reply struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	var content string
	if len(reply.Choices) == 1 {
		content = reply.Choices[0].Message.Content
	} else if len(reply.Content) == 1 {
		content = reply.Content[0].Text
	}
	var captured canonicalExecution
	if err := json.Unmarshal([]byte(content), &captured); err != nil {
		t.Fatalf("missing canonical execution evidence: %s (%v)", response.Body, err)
	}
	return captured
}

func TestSDKCanonicalCallerThroughFront(t *testing.T) {
	id := installCanonicalExtensionFixture(t)
	_, secrets := newCaller(t, "Canonical one", "Canonical two")
	server := New()
	t.Cleanup(server.idle.stop)
	handler := server.Front()
	var callers []string
	for _, sample := range []struct {
		name, path, secret string
	}{
		{"first key Chat", "/v1/chat/completions", secrets[0]},
		{"same key Messages", "/v1/messages", secrets[0]},
		{"same key spoof", "/v1/chat/completions", secrets[0]},
		{"other key", "/v1/chat/completions", secrets[1]},
		{"local owner Chat", "/v1/chat/completions", ""},
		{"local owner Messages", "/v1/messages", ""},
	} {
		t.Run(sample.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, sample.path, strings.NewReader(
				`{"model":"`+id+`/m1","stream":false,"max_tokens":64,"messages":[{"role":"user","content":"hello"}],`+
					`"callerId":"body-spoof","options":{"callerId":"options-spoof","requestId":"spoof","native":{"private":true}}}`))
			request.RemoteAddr = "127.0.0.1:12345"
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Magpie-Caller-ID", "header-spoof")
			if sample.secret != "" {
				request.Header.Set("Authorization", "Bearer "+sample.secret)
			}
			ctx := extensionhost.WithRequestMetadata(request.Context(), extensionhost.RequestMetadata{
				Plugin: "foreign.module", CallerID: "foreign-spoof", RequestID: "foreign-request",
				Value: json.RawMessage(`{"private":"foreign"}`),
			})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request.WithContext(ctx))
			captured := readCanonicalExecution(t, response)
			var options sdk.ExecuteOptions
			if err := json.Unmarshal(captured.Request.Options, &options); err != nil {
				t.Fatal(err)
			}
			callers = append(callers, options.CallerID)
			if options.CallerID == "" || strings.Contains(options.CallerID, "spoof") {
				t.Errorf("ordinary canonical caller was missing or client-controlled: %q", options.CallerID)
			}
			if options.RequestID != "" || len(options.Native) != 0 {
				t.Errorf("private metadata crossed owner boundary: %+v", options)
			}
		})
	}
	if len(callers) != 6 {
		t.Fatalf("missing caller observations: %v", callers)
	}
	if callers[0] != callers[1] || callers[0] != callers[2] || callers[0] == callers[3] ||
		callers[4] != callers[5] || callers[4] == callers[0] || callers[4] == callers[3] {
		t.Fatalf("trusted caller identity did not remain stable and isolated: %v", callers)
	}
}

func TestSDKEffectiveModelReachesRouteAndLedger(t *testing.T) {
	id := installCanonicalExtensionFixture(t, "wire-variant", "wire-matching", "wire-spoof", "wire-invalid")
	server := New()
	t.Cleanup(server.idle.stop)
	handler := server.Front()
	expected := map[string]string{
		"wire-variant":  "vendor/wire-variant-high",
		"wire-matching": "",
		"wire-spoof":    "",
		"wire-invalid":  "",
	}
	for model, wire := range expected {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
			`{"model":"`+id+`/`+model+`","messages":[{"role":"user","content":"hello"}]}`))
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		execution := readCanonicalExecution(t, response)
		if execution.Request.Model != model {
			t.Errorf("the SDK received %q, want %q", execution.Request.Model, model)
		}
		for header := range response.Header() {
			if strings.EqualFold(header, provider.SentModelHeader) {
				t.Errorf("%s leaked an internal header: %s", model, header)
			}
		}
		if tries := lastRoute(server).Tries; len(tries) != 1 || tries[0].Wire != wire {
			t.Errorf("%s: routing trace should keep %q, got %+v", model, wire, tries)
		}
	}
	rows := usage.Load(time.Time{})
	if len(rows) != len(expected) {
		t.Fatalf("have %d ledger records, want %d", len(rows), len(expected))
	}
	for _, row := range rows {
		if wire, ok := expected[row.Model]; !ok || row.Wire != wire {
			t.Errorf("unexpected ledger record model=%q wire=%q", row.Model, row.Wire)
		}
	}
}

func TestSDKCanonicalChatHistoryThroughFront(t *testing.T) {
	id := installCanonicalExtensionFixture(t)
	server := New()
	t.Cleanup(server.idle.stop)
	handler := server.Front()
	first := `{"role":"assistant","content":null,"reasoning":"First a.","tool_calls":[` + readCall("a") + `]}`
	second := `{"role":"assistant","content":"reading","reasoning_content":"Then b.","tool_calls":[` + readCall("b") + `]}`
	sameAlias := strings.Replace(first, `"reasoning":`, `"reasoning_content":`, 1)
	results := `{"role":"tool","tool_call_id":"a","content":"real A"},{"role":"tool","tool_call_id":"b","content":"real B"}`
	for _, sample := range []struct{ name, turns string }{
		{"mixed reasoning aliases", first + "," + second + "," + results},
		{"missing real tool result", sameAlias + "," + second + `,{"role":"tool","tool_call_id":"a","content":"real A"}`},
		{"distinct cache boundaries", strings.Replace(sameAlias, `"content":null`, `"content":null,"cache_control":{"type":"ephemeral"}`, 1) +
			"," + strings.Replace(second, `"content":"reading"`, `"content":"reading","cache_control":{"type":"ephemeral","ttl":"1h"}`, 1) + "," + results},
		{"same alias positive", sameAlias + "," + second + "," + results},
		{"paired positive", `{"role":"assistant","content":null,"tool_calls":[` + readCall("a") +
			`, ` + readCall("b") + `]},` + results},
	} {
		t.Run(sample.name, func(t *testing.T) {
			messages := `[{"role":"user","content":"read both"},` + sample.turns + `]`
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"`+id+`/m1","stream":false,"messages":`+messages+`}`))
			request.RemoteAddr = "127.0.0.1:12345"
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			captured := readCanonicalExecution(t, response)
			var actual struct {
				Messages []map[string]any `json:"messages"`
			}
			var expected []map[string]any
			if err := json.Unmarshal(captured.Request.Body, &actual); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(messages), &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual.Messages, expected) {
				t.Errorf("SDK caller history changed before execution:\ngot  %s\nwant %s", captured.Request.Body, messages)
			}
		})
	}
}

func TestSDKCanonicalHistoryKeepsLegacyRepairForNonSDK(t *testing.T) {
	upstream := &fake{t: t, reply: chatWholeOK, ctype: "application/json", refuse: kimiRefusePairs}
	setup(t, provider.Chat, upstream)
	server := New()
	t.Cleanup(server.idle.stop)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"fake/m1","stream":false,"messages":[{"role":"user","content":"read both"},`+
			`{"role":"assistant","content":null,"tool_calls":[`+readCall("a")+`]},`+
			`{"role":"assistant","content":"reading","tool_calls":[`+readCall("b")+`]},`+
			`{"role":"tool","tool_call_id":"a","content":"real A"}]}`))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Front().ServeHTTP(response, request)
	if response.Code != http.StatusOK || upstream.calls != 1 {
		t.Fatalf("non-SDK legacy repair failed: status=%d calls=%d body=%s", response.Code, upstream.calls, response.Body)
	}
	var captured struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(upstream.got, &captured); err != nil {
		t.Fatal(err)
	}
	if relayTrace(captured.Messages) != "user assistant(a,b) tool(a) tool(b)" ||
		!strings.Contains(string(upstream.got), "the turn was interrupted") {
		t.Fatalf("non-SDK lost its legacy Chat repair: %s", upstream.got)
	}
}
