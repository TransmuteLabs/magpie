package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

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
	module := sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{
		sdk.MethodProviderExecute: func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
			var request sdk.ExecuteRequest
			if err := json.Unmarshal(call.Params, &request); err != nil {
				return nil, err
			}
			if strings.Contains(string(request.Body), "not-started") {
				return nil, &sdk.Error{Code: "not_started", Message: "fixture did not start", Outcome: "not-started"}
			}
			if strings.Contains(string(request.Body), "completed success") {
				if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: http.StatusOK,
					Headers: http.Header{"Content-Type": {"application/json"}}}); err != nil {
					return nil, err
				}
				body, err := json.Marshal(map[string]any{
					"id": "external-success", "model": request.Model,
					"choices": []any{map[string]any{"message": map[string]any{"role": "assistant",
						"content": "native success: " + call.Scope.Operation}, "finish_reason": "stop"}},
					"usage": map[string]int{"prompt_tokens": 7, "completion_tokens": 2},
				})
				if err != nil {
					return nil, err
				}
				if err := stream.Emit(ctx, "data", body); err != nil {
					return nil, err
				}
				return sdk.ExecuteResult{Status: http.StatusOK, Outcome: "completed"}, nil
			}
			if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
				return nil, err
			}
			if strings.Contains(string(request.Body), "death") {
				os.Exit(23)
			}
			if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: http.StatusBadGateway,
				Headers: http.Header{"Content-Type": {"application/json"}}}); err != nil {
				return nil, err
			}
			if err := stream.Emit(ctx, "data", []byte(`{"error":{"message":"accepted operation failed"}}`)); err != nil {
				return nil, err
			}
			return sdk.ExecuteResult{Status: http.StatusBadGateway, Outcome: "failed"}, nil
		},
	}}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
