package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

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
	account := call.Scope.Account
	contentType, body := "application/json", []byte{}
	switch request.Operation {
	case sdk.OperationEmbeddings:
		body = []byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.25,0.5]}],"usage":{"prompt_tokens":2}}`)
	case sdk.OperationRerank:
		body = []byte(`{"results":[{"index":0,"relevance_score":0.9}]}`)
	case sdk.OperationAudioSpeech:
		contentType, body = "audio/wav", append([]byte{0, 255}, []byte("audio:"+account)...)
	case sdk.OperationAudioTranscribe, sdk.OperationAudioTranslate:
		body = []byte(`{"text":"fixture transcript"}`)
	case sdk.OperationImageGenerate, sdk.OperationImageEdit:
		body = []byte(`{"data":[{"b64_json":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg=="}]}`)
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
