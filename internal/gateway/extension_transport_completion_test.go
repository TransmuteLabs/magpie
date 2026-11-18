package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const extensionCompletionWorker = `package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func main() {
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err != nil { panic(err) }
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil { panic(err) }
	module := sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{
		sdk.MethodProviderExecute: func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
			var request sdk.ExecuteRequest
			if err := json.Unmarshal(call.Params, &request); err != nil { return nil, err }
			var body struct { Messages []struct { Content string } }
			if err := json.Unmarshal(request.Body, &body); err != nil { return nil, err }
			mode := body.Messages[0].Content
			if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil { return nil, err }
			if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: http.StatusOK,
				Headers: http.Header{"Content-Type": {"text/event-stream"}}}); err != nil { return nil, err }
			start := "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n"
			finish := "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
			switch mode {
			case "parse-error":
				start, finish = "data: {\"choices\":[INVALID]}\n\n", ""
			case "parse-before-terminal":
				start = "data: {\"choices\":[INVALID]}\n\n" + start
			case "typed-error-before-terminal":
				start = "data: {\"choices\":\"invalid\"}\n\n" + start
			case "typed-error-in-terminal":
				finish = "data: {\"id\":{},\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
			case "trailing-invalid-envelope":
				finish += "data: {\"choices\":[INVALID]}\n\n"
			case "stream-error":
				finish = "data: {\"error\":{\"message\":\"refused\"}}\n\n"
			case "eof-only":
				finish = ""
			}
			if mode != "eof-only" && mode != "parse-error" {
				finish += "data: [DONE]\n\n"
			}
			if err := stream.Emit(ctx, "data", []byte(start + finish)); err != nil { return nil, err }
			if mode == "read-error" {
				return nil, &sdk.Error{Code: "response_cut", Message: "fixture read failed", Outcome: "uncertain"}
			}
			if err := stream.Emit(ctx, "data", []byte(":"+strings.Repeat(" ", 4096)+"\n\n")); err != nil { return nil, err }
			return sdk.ExecuteResult{Status: http.StatusOK, Outcome: "completed",
				Usage: &sdk.Usage{InputTokens: 13, OutputTokens: 6, CachedTokens: 4}}, nil
		},
	}}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil { panic(err) }
}
`

func TestExtensionTranslatedReadersConfirmOnlyDecodedTerminal(test *testing.T) {
	directory := test.TempDir()
	manifest := extensionGatewayManifest()
	manifest.ID = "fixture.completion"
	source := filepath.Join(test.TempDir(), "main.go")
	if err := os.WriteFile(source, []byte(extensionCompletionWorker), 0o600); err != nil {
		test.Fatal(err)
	}
	buildContext, cancelBuild := context.WithTimeout(test.Context(), time.Minute)
	defer cancelBuild()
	build := proc.CommandContext(buildContext, filepath.Join(runtime.GOROOT(), "bin", "go"),
		"build", "-mod=readonly", "-o", filepath.Join(directory, manifest.Entry), source)
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		test.Fatalf("build completion fixture: %v\n%s", err, output)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0o600); err != nil {
		test.Fatal(err)
	}
	for _, reader := range []string{"whole", "stream", "search"} {
		for _, mode := range []string{"terminal", "eof-only", "parse-error", "parse-before-terminal", "typed-error-before-terminal", "typed-error-in-terminal", "trailing-invalid-envelope", "stream-error", "read-error"} {
			test.Run(reader+"/"+mode, func(test *testing.T) {
				fresh(test)
				test.Setenv("MAGPIE_NO_ACCOUNTS", "")
				test.Setenv("MAGPIE_ADDR", "")
				manager := extensionhost.Default()
				test.Cleanup(func() { closeSDKGatewayHook(test, manager) })
				if _, err := manager.Install(test.Context(), directory); err != nil {
					test.Fatal(err)
				}
				provider.Changed()
				all, err := provider.ExtensionProviders()
				if err != nil || len(all) != 1 {
					test.Fatalf("fixture provider was not projected: %v %v", all, err)
				}
				selected := all[0]
				ctx, attempt := provider.WithExtensionAttempt(test.Context())
				server := New()
				test.Cleanup(server.idle.stop)
				var status int
				var message string
				if reader == "search" {
					request, err := parseChat([]byte(`{"model":"m1","messages":[{"role":"user","content":"` + mode + `"}]}`))
					if err != nil {
						test.Fatal(err)
					}
					events, code, failure := server.askTranslated(selected, provider.Chat, "m1", http.Header{}, http.Header{})(ctx, request)
					status, message = code, failure
					if events == nil {
						test.Fatalf("search round did not reach its reader: %d %s", status, message)
					}
					for event := range events {
						if event.Kind == KError {
							message += event.Text
						}
					}
				} else {
					stream := "false"
					if reader == "stream" {
						stream = "true"
					}
					requestBody := `{"model":"m1","input":"` + mode + `","stream":` + stream + `}`
					request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody)).WithContext(ctx)
					status, message = server.translate(httptest.NewRecorder(), request, selected, provider.Responses, provider.Chat, "m1", []byte(requestBody), &Usage{})
				}
				module, operation := attempt.Operation()
				journal, err := manager.Store().Operation(module, operation)
				completed := mode != "read-error" && (reader != "stream" ||
					mode == "terminal" || mode == "eof-only" || mode == "parse-error")
				if err != nil || attempt.Outcome(true) == nil {
					test.Fatalf("reader lost its terminal attempt or allowed replay: state=%s error=%v outcome=%v",
						journal.State, err, attempt.Outcome(true))
				}
				if completed {
					if journal.State != "completed" || attempt.Outcome(false) != nil {
						test.Fatalf("reader lost its authoritative receipt: state=%s status=%d message=%s outcome=%v",
							journal.State, status, message, attempt.Outcome(false))
					}
				} else if attempt.Outcome(false) == nil {
					test.Fatalf("rejected decoder authorized a tail drain: state=%s status=%d message=%s",
						journal.State, status, message)
				}
			})
		}
	}
}

func TestExtensionProtocolDecoderRecordsRejection(test *testing.T) {
	for _, fixture := range []struct {
		protocol        provider.Protocol
		before          string
		terminal        string
		invalidTerminal string
	}{
		{provider.Chat, `{"choices":"invalid"}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"id":{},"choices":[{"delta":{},"finish_reason":"stop"}]}`},
		{provider.Responses, `{"response":{"id":{}}}`,
			`{"type":"response.completed","response":{"status":"completed"}}`,
			`{"type":"response.completed","response":{"id":{},"status":"completed"}}`},
		{provider.Anthropic, `{"index":"invalid"}`,
			`{"type":"message_stop"}`,
			`{"type":"message_stop","message":{"id":{}}}`},
		{provider.Gemini, `{"candidates":"invalid"}`,
			`{"candidates":[{"finishReason":"STOP"}]}`,
			`{"modelVersion":{},"candidates":[{"finishReason":"STOP"}]}`},
		{provider.CodeAssist, `{"response":{"candidates":"invalid"}}`,
			`{"response":{"candidates":[{"finishReason":"STOP"}]}}`,
			`{"response":{"modelVersion":{},"candidates":[{"finishReason":"STOP"}]}}`},
	} {
		for _, scenario := range []struct {
			name     string
			data     []string
			rejected bool
		}{
			{"valid", []string{`{"type":"unknown"}`, fixture.terminal}, false},
			{"syntax-before-terminal", []string{`{"INVALID"`, fixture.terminal}, true},
			{"typed-before-terminal", []string{fixture.before, fixture.terminal}, true},
			{"typed-terminal", []string{fixture.invalidTerminal, fixture.terminal}, true},
		} {
			test.Run(string(fixture.protocol)+"/"+scenario.name, func(test *testing.T) {
				var decoding protocolDecodeState
				decode := carryReasoning(decoderFor(provider.Provider{}, fixture.protocol, nil, &decoding),
					provider.Chat, fixture.protocol)
				for _, data := range scenario.data {
					if err := decode(data, func(Event) {}); err != nil {
						test.Fatalf("completion observation changed tolerant decoding: %v", err)
					}
				}
				if decoding.rejected != scenario.rejected {
					test.Fatalf("decoder rejection lost across later terminal: rejected=%v want=%v",
						decoding.rejected, scenario.rejected)
				}
			})
		}
	}
}
