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
			if request.Protocol == "responses" {
				return streamResponsesFixture(ctx, call, stream, request)
			}
			bodyText := string(request.Body)
			if manifest.ID == "opencode" && strings.Contains(bodyText, "project-fixture-") {
				code := "project_required"
				switch {
				case strings.Contains(bodyText, "project-fixture-malformed"):
					code = "invalid_project"
				case strings.Contains(bodyText, "project-fixture-unrelated"):
					code = "invalid_params"
				case strings.Contains(bodyText, "project-fixture-accepted"):
					if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
						return nil, err
					}
				}
				return nil, &sdk.Error{Code: code, Message: "Explicit OpenCode projectId is required", Outcome: "not-started"}
			}
			if strings.Contains(bodyText, "not-started") ||
				strings.Contains(bodyText, "accepted effort failure") && strings.Contains(bodyText, "The number of the kind that fits it best") {
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

func streamResponsesFixture(ctx context.Context, call sdk.Call, stream *sdk.Stream, request sdk.ExecuteRequest) (any, error) {
	const prefix = "sdk socket output "
	body := string(request.Body)
	if strings.Contains(body, "and more") && !strings.Contains(body, prefix) {
		return nil, &sdk.Error{Code: "missing_history", Message: "fixture continuation lost its assistant item", Outcome: "not-started"}
	}
	if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
		return nil, err
	}
	if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: http.StatusOK,
		Headers: http.Header{"Content-Type": {"text/event-stream"}}}); err != nil {
		return nil, err
	}
	id := call.Scope.Operation
	text := prefix + strings.Repeat("x", 128<<10)
	item := fmt.Sprintf(`{"type":"message","id":"msg_fixture","role":"assistant","phase":"final_answer","status":"completed","content":[{"type":"output_text","text":%q}]}`, text)
	for _, event := range []string{
		fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"model":%q,"object":"response","status":"in_progress","output":[]}}`, id, request.Model),
		`{"type":"response.output_text.delta","item_id":"msg_fixture","output_index":0,"content_index":0,"delta":"sdk socket output "}`,
		`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
		fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"model":%q,"object":"response","status":"completed","output":[%s],"usage":{"input_tokens":11,"output_tokens":5,"total_tokens":16}}}`, id, request.Model, item),
	} {
		wire := []byte("data: " + event + "\n\n")
		for len(wire) > 0 {
			size := min(len(wire), sdk.MaxChunkBytes)
			if err := stream.Emit(ctx, "data", wire[:size]); err != nil {
				return nil, err
			}
			wire = wire[size:]
		}
	}
	return sdk.ExecuteResult{Status: http.StatusOK, Outcome: "completed", Usage: &sdk.Usage{InputTokens: 13, OutputTokens: 6}}, nil
}
