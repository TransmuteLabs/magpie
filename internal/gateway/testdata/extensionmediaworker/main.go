package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

var partialDraws atomic.Int32

func execute(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var request sdk.ExecuteRequest
	if err := json.Unmarshal(call.Params, &request); err != nil {
		return nil, err
	}
	if request.Headers.Get("Authorization") != "" || len(request.Credentials) > 0 {
		return nil, &sdk.Error{Code: "invalid_scope", Message: "fixture saw authority outside auth", Outcome: "not-started"}
	}
	if strings.Contains(string(request.Body), "accepted failure") {
		if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
			return nil, err
		}
		return nil, &sdk.Error{Code: "fixture_failed", Message: "fixture failed after acceptance", Outcome: "accepted"}
	}
	if strings.Contains(string(request.Body), "not started normalized usage") {
		if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: http.StatusServiceUnavailable}); err != nil {
			return nil, err
		}
		if err := stream.Emit(ctx, "data", []byte(`{"error":{"message":"fixture unavailable"}}`)); err != nil {
			return nil, err
		}
		if err := stream.Emit(ctx, "usage", sdk.Usage{InputTokens: 99}); err != nil {
			return nil, err
		}
		return sdk.ExecuteResult{Status: http.StatusServiceUnavailable, Outcome: "not-started"}, nil
	}
	if strings.Contains(string(request.Body), "terminal media") {
		status := http.StatusTooManyRequests
		message := "model quota exhausted"
		if strings.Contains(string(request.Body), "terminal media 404") {
			status = http.StatusNotFound
			message = "model is not supported by any configured account in this group"
		}
		if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: status}); err != nil {
			return nil, err
		}
		failure, _ := json.Marshal(map[string]any{"error": map[string]string{"message": message}})
		if err := stream.Emit(ctx, "data", failure); err != nil {
			return nil, err
		}
		return sdk.ExecuteResult{Status: status, Outcome: "completed", Usage: &sdk.Usage{InputTokens: 17}}, nil
	}
	if strings.Contains(string(request.Body), "partial normalized usage") && partialDraws.Add(1)%2 == 0 {
		if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
			return nil, err
		}
		if err := stream.Emit(ctx, "usage", sdk.Usage{InputTokens: 13, OutputTokens: 7}); err != nil {
			return nil, err
		}
		return nil, &sdk.Error{Code: "fixture_failed", Message: "one drawing failed after acceptance", Outcome: "uncertain"}
	}
	account := call.Scope.Account
	contentType, body := "application/json", []byte{}
	switch request.Operation {
	case "":
		if request.Protocol == "chat" {
			body = []byte(`{"choices":[{"message":{"content":"","images":[{"image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg=="}}]}}]}`)
			break
		}
		if request.Protocol != "gemini" {
			return nil, fmt.Errorf("expected Gemini image protocol, got %s", request.Protocol)
		}
		body = []byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg=="}}]}}]}`)
	case sdk.OperationEmbeddings:
		body = []byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.25,0.5]}],"usage":{"prompt_tokens":2}}`)
		if strings.Contains(string(request.Body), "fixture caller label") {
			var result map[string]any
			if err := json.Unmarshal(body, &result); err != nil {
				return nil, err
			}
			result["fixture_caller"] = request.Caller
			result["fixture_caller_headers"] = request.Headers.Get("X-Magpie-Caller-Agent") != "" ||
				request.Headers.Get("X-Magpie-Caller-Name") != "" || request.Headers.Get("X-Magpie-Caller-Via") != ""
			var err error
			body, err = json.Marshal(result)
			if err != nil {
				return nil, err
			}
		}
	case sdk.OperationRerank:
		body = []byte(`{"results":[{"index":0,"relevance_score":0.9}]}`)
	case sdk.OperationAudioSpeech:
		contentType, body = "audio/wav", append([]byte{0, 255}, []byte("audio:"+account)...)
	case sdk.OperationAudioTranscribe, sdk.OperationAudioTranslate:
		body = []byte(`{"text":"fixture transcript"}`)
	case sdk.OperationImageGenerate, sdk.OperationImageEdit:
		image := map[string]string{"b64_json": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg=="}
		if strings.Contains(string(request.Body), "echo image request") {
			image["revised_prompt"] = string(request.Body)
		}
		body, _ = json.Marshal(map[string]any{"data": []map[string]string{image}})
	case sdk.OperationVideoCreate, sdk.OperationVideoGet, sdk.OperationVideoRemix:
		body, _ = json.Marshal(map[string]any{"id": "same-video", "model": "fixture-movie", "status": "completed", "fixture_account": account})
	case sdk.OperationVideoContent:
		contentType, body = "video/mp4", []byte("video:"+account)
	case sdk.OperationVideoDelete:
		body = []byte(`{"deleted":true}`)
	default:
		return nil, &sdk.Error{Code: "unsupported", Message: "fixture has no such operation", Outcome: "not-started"}
	}
	if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: http.StatusOK, Headers: http.Header{"Content-Type": {contentType}}}); err != nil {
		return nil, err
	}
	if err := stream.Emit(ctx, "data", body); err != nil {
		return nil, err
	}
	if strings.Contains(string(request.Body), "rich normalized usage") {
		pointer := func(value int64) *int64 { return &value }
		text := func(value string) *string { return &value }
		credits := 0.25
		currency := "USD"
		if strings.Contains(string(request.Body), "rich normalized usage EUR") {
			currency = "EUR"
		}
		return sdk.ExecuteResult{Status: http.StatusOK, Outcome: "completed", Usage: &sdk.Usage{Facts: &sdk.ExecutionUsageFacts{
			ExecutionCounts: sdk.ExecutionCounts{InputTokens: pointer(17), OutputTokens: pointer(5), CachedTokens: pointer(4),
				CacheWriteTokens: pointer(7), CacheWrite1hTokens: pointer(3), ReasoningTokens: pointer(2)},
			Deferred:    &sdk.ExecutionCounts{InputTokens: pointer(11), OutputTokens: pointer(1)},
			ServedModel: text("served-fixture"), UpstreamProvider: text("fixture-upstream"), Tier: text("flex"),
			RequestID: text("fixture-request"), ResponseID: text("fixture-response"), Stop: text("completed"),
			Credits: &credits, Cost: &sdk.ExecutionCost{Amount: 0.002, Currency: currency},
		}}}, nil
	}
	if strings.Contains(string(request.Body), "normalized usage") {
		totals := sdk.Usage{InputTokens: 11, OutputTokens: 3, CachedTokens: 2, CacheWriteTokens: 1}
		if err := stream.Emit(ctx, "usage", totals); err != nil {
			return nil, err
		}
		if strings.Contains(string(request.Body), "late normalized usage failure") {
			if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
				return nil, err
			}
			return nil, &sdk.Error{Code: "fixture_failed", Message: "fixture failed after audio data", Outcome: "uncertain"}
		}
		if strings.Contains(string(request.Body), "normalized usage event only") {
			return sdk.ExecuteResult{Status: http.StatusOK, Outcome: "completed"}, nil
		}
		totals = sdk.Usage{InputTokens: 17, OutputTokens: 5, CachedTokens: 4, CacheWriteTokens: 2}
		if strings.Contains(string(request.Body), "normalized usage zero") {
			totals = sdk.Usage{}
		}
		return sdk.ExecuteResult{Status: http.StatusOK, Outcome: "completed", Usage: &totals}, nil
	}
	return sdk.ExecuteResult{Status: http.StatusOK, Outcome: "completed"}, nil
}

func main() {
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	unsupported := func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
		return nil, &sdk.Error{Code: "unsupported", Message: "fixture auth is preprovisioned", Outcome: "not-started"}
	}
	refresh := func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
		credentials, _ := json.Marshal(map[string]string{"access": "rotated-" + call.Scope.Account})
		return sdk.AuthResult{ID: call.Scope.Account, Credentials: credentials, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, nil
	}
	module := sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{
		sdk.MethodProviderExecute: execute,
		sdk.MethodAuthStart:       unsupported, sdk.MethodAuthFinish: unsupported,
		sdk.MethodAuthRefresh: refresh, sdk.MethodAuthLogout: unsupported,
	}}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
