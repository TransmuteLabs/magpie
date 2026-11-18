package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
	codec "github.com/yetone/magpie/pkg/protocolext"
)

type inbound struct{}

func (inbound) ID() string               { return "executor" }
func (inbound) Path() string             { return "/executor.fixture/Call" }
func (inbound) Contract() codec.Contract { return codec.ExecutorContract() }
func (inbound) Decode(_ context.Context, _ http.Header, body []byte) (codec.Request, error) {
	var input struct {
		Model    string               `json:"model"`
		Content  string               `json:"content"`
		Executor codec.ExecutorPolicy `json:"executor"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		return codec.Request{}, err
	}
	canonical, err := json.Marshal(map[string]any{
		"model": input.Model, "messages": []any{map[string]string{"role": "user", "content": input.Content}},
	})
	return codec.Request{
		Protocol: codec.Chat, Body: canonical, Executor: input.Executor,
		Metadata: json.RawMessage(`{"fixture":"native"}`),
	}, err
}
func (inbound) Reply(writer http.ResponseWriter) codec.Response { return response{writer} }

type response struct{ http.ResponseWriter }

func (response) Finish(error) error { return nil }
func (reply response) Flush()       { _ = http.NewResponseController(reply.ResponseWriter).Flush() }

func execute(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var request sdk.ExecuteRequest
	if err := json.Unmarshal(call.Params, &request); err != nil {
		return nil, err
	}
	var canonical struct {
		Messages []struct{ Content string }
	}
	if err := json.Unmarshal(request.Body, &canonical); err != nil || len(canonical.Messages) != 1 {
		return nil, &sdk.Error{Code: "not_started", Message: "invalid fixture request", Outcome: "not-started"}
	}
	if canonical.Messages[0].Content == "refuse" {
		return nil, &sdk.Error{Code: "not_started", Message: "fixture refused before dispatch", Outcome: "not-started"}
	}
	var options sdk.ExecuteOptions
	if err := json.Unmarshal(request.Options, &options); err != nil {
		return nil, err
	}
	if call.Scope.Plugin == "fixture.executor" && string(options.Native) != `{"fixture":"native"}` {
		return nil, &sdk.Error{Code: "not_started", Message: "native metadata was lost", Outcome: "not-started"}
	}
	body, err := json.Marshal(map[string]any{
		"id": "executor-fixture", "model": request.Model,
		"choices": []any{map[string]any{"message": map[string]string{
			"role": "assistant", "content": call.Scope.Plugin,
		}, "finish_reason": "stop"}},
		"usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 1},
	})
	if err != nil {
		return nil, err
	}
	if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: http.StatusOK,
		Headers: http.Header{"Content-Type": {"application/json"}}}); err != nil {
		return nil, err
	}
	if err := stream.Emit(ctx, "data", body); err != nil {
		return nil, err
	}
	return sdk.ExecuteResult{Status: http.StatusOK, Outcome: "completed"}, nil
}

func main() {
	raw, err := os.ReadFile(sdk.ManifestFile)
	var manifest sdk.Manifest
	if err != nil || json.Unmarshal(raw, &manifest) != nil {
		fmt.Fprintln(os.Stderr, "invalid fixture manifest")
		os.Exit(1)
	}
	module := sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{sdk.MethodProviderExecute: execute}}
	if manifest.Contract.Has(sdk.InboundCodec) {
		module.Inbounds = []codec.Inbound{inbound{}}
	}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
