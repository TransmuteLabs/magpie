// This is an independently executed plugin. It imports only the public SDK
// and standard library and never contacts a vendor.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
	codec "github.com/yetone/magpie/pkg/protocolext"
)

//go:embed magpie-plugin.json
var declaration []byte

func main() {
	var manifest sdk.Manifest
	if err := json.Unmarshal(declaration, &manifest); err != nil {
		panic(err)
	}
	module := sdk.Module{
		Manifest: manifest,
		Inbounds: []codec.Inbound{nativeEcho{}},
		Handlers: map[string]sdk.Handler{
			sdk.MethodProviderExecute: execute,
			sdk.MethodProviderModels: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
				return sdk.ModelsResult{Models: manifest.Providers[0].Models}, nil
			},
			sdk.MethodToolCall: func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
				var request sdk.ToolRequest
				if err := json.Unmarshal(call.Params, &request); err != nil {
					return nil, err
				}
				var arguments struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(request.Arguments, &arguments); err != nil {
					return nil, err
				}
				content, err := json.Marshal([]map[string]string{{"type": "text", "text": arguments.Text}})
				return sdk.ToolResult{Content: content}, err
			},
		},
	}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil {
		fmt.Fprintln(os.Stderr, "SDK echo worker stopped")
		os.Exit(1)
	}
}

func execute(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var request sdk.ExecuteRequest
	if err := json.Unmarshal(call.Params, &request); err != nil {
		return nil, err
	}
	if request.Protocol != "chat" {
		return nil, &sdk.Error{Code: "unsupported", Message: "This example accepts Chat Completions only.", Outcome: "not-started"}
	}
	var chat struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(request.Body, &chat); err != nil {
		return nil, &sdk.Error{Code: "invalid_params", Message: "A canonical Chat request is required.", Outcome: "not-started"}
	}
	text := "hello"
	if len(chat.Tools) > 0 {
		return nil, &sdk.Error{Code: "unsupported", Message: "The echo example does not implement model tool calls.", Outcome: "not-started"}
	}
	if len(chat.Messages) > 0 {
		// The example echoes string content; it leaves multimodal parsing to
		// an author's actual provider implementation.
		if json.Unmarshal(chat.Messages[len(chat.Messages)-1].Content, &text) != nil {
			return nil, &sdk.Error{Code: "unsupported", Message: "The echo example accepts a text message only.", Outcome: "not-started"}
		}
	}
	if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
		return nil, err
	}
	contentType := "application/json"
	if chat.Stream {
		contentType = "text/event-stream"
	}
	if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: 200, Headers: http.Header{"Content-Type": {contentType}}}); err != nil {
		return nil, err
	}
	var body []byte
	if chat.Stream {
		delta, _ := json.Marshal(map[string]any{
			"id": "sdk-echo", "object": "chat.completion.chunk", "model": request.Model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"role": "assistant", "content": text}, "finish_reason": nil}},
		})
		terminal, _ := json.Marshal(map[string]any{
			"id": "sdk-echo", "object": "chat.completion.chunk", "model": request.Model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		})
		body = append([]byte("data: "), delta...)
		body = append(body, []byte("\n\ndata: ")...)
		body = append(body, terminal...)
		body = append(body, []byte("\n\ndata: [DONE]\n\n")...)
	} else {
		body, _ = json.Marshal(map[string]any{
			"id": "sdk-echo", "object": "chat.completion", "model": request.Model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": text}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}
	if len(body) > sdk.MaxChunkBytes {
		return nil, errors.New("example reply is too large")
	}
	if err := stream.Emit(ctx, "data", body); err != nil {
		return nil, err
	}
	if chat.Stream {
		if err := stream.Emit(ctx, "flush", nil); err != nil {
			return nil, err
		}
	}
	return sdk.ExecuteResult{Status: 200, Outcome: "completed", Usage: &sdk.Usage{InputTokens: 1, OutputTokens: 1}}, nil
}

type nativeEcho struct{}

func (nativeEcho) ID() string               { return "echo" }
func (nativeEcho) Path() string             { return "/sdk-echo/rpc" }
func (nativeEcho) Contract() codec.Contract { return codec.HostContract() }
func (nativeEcho) Decode(ctx context.Context, headers http.Header, body []byte) (codec.Request, error) {
	var request struct {
		Model     string          `json:"model"`
		Prompt    string          `json:"prompt"`
		Stream    bool            `json:"stream"`
		Models    bool            `json:"models"`
		RequestID string          `json:"requestId"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return codec.Request{}, err
	}
	if request.Models {
		return codec.Request{Protocol: codec.Models}, nil
	}
	if request.Tool != "" {
		if len(request.Arguments) == 0 {
			request.Arguments = json.RawMessage(`{}`)
		}
		body, err := json.Marshal(sdk.ToolRequest{ID: request.Tool, Arguments: request.Arguments})
		return codec.Request{Protocol: codec.Tool, Body: body, RequestID: request.RequestID}, err
	}
	canonical, err := json.Marshal(map[string]any{"model": request.Model, "stream": request.Stream,
		"messages": []map[string]string{{"role": "user", "content": request.Prompt}}})
	return codec.Request{Protocol: codec.Chat, Body: canonical, RequestID: request.RequestID}, err
}
func (nativeEcho) Reply(writer http.ResponseWriter) codec.Response { return nativeReply{writer} }

type nativeReply struct{ http.ResponseWriter }

func (nativeReply) Finish(error) error { return nil }
