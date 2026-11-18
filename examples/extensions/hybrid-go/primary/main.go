package main

import (
	"context"
	"encoding/json"

	hybrid "github.com/yetone/magpie/examples/extensions/hybrid-go"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/extension/native"
)

func init() {
	manifest := hybrid.Manifest()
	if err := native.Register(sdk.Module{
		Manifest: manifest,
		Actions: map[string]sdk.Handler{
			"pid": hybrid.Read, "forward": forward,
		},
		Handlers: map[string]sdk.Handler{
			sdk.MethodToolCall: tool,
			sdk.MethodProviderModels: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
				return sdk.ModelsResult{Models: manifest.Providers[0].Models}, nil
			},
			sdk.MethodAuthStart: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
				return sdk.AuthStartResult{Continuation: json.RawMessage(`{"fixture":true}`)}, nil
			},
			sdk.MethodAuthFinish: finishAuth,
			sdk.MethodAuthRefresh: func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
				return sdk.AuthResult{ID: call.Scope.Account, Credentials: call.Auth}, nil
			},
			sdk.MethodAuthLogout: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
				return map[string]bool{"ok": true}, nil
			},
		},
	}); err != nil {
		panic(err)
	}
}

func main() {}

func forward(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	if err := hybrid.Accept(ctx, call, stream); err != nil {
		return nil, err
	}
	return forwardAccepted(ctx, call, stream)
}

func forwardAccepted(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var input hybrid.Input
	if err := json.Unmarshal(call.Params, &input); err != nil {
		return nil, err
	}
	primary, err := hybrid.Describe(ctx, call, input.Text)
	if err != nil {
		return nil, err
	}
	var backend hybrid.Identity
	if err := stream.HostCall(ctx, sdk.MethodComponentCall, sdk.ComponentCallRequest{
		Binding: "controller.run", Input: call.Params,
	}, &backend); err != nil {
		return nil, err
	}
	return hybrid.ForwardResult{Primary: primary, Backend: backend}, nil
}

func tool(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var request sdk.ToolRequest
	if err := json.Unmarshal(call.Params, &request); err != nil {
		return nil, err
	}
	if request.ID != "stream" && request.ID != "wait" && request.ID != "forward" {
		return nil, &sdk.Error{Code: "invalid_params", Message: "Unknown hybrid tool.", Outcome: "not-started"}
	}
	var input hybrid.Input
	if err := json.Unmarshal(request.Arguments, &input); err != nil {
		return nil, err
	}
	if err := hybrid.Accept(ctx, call, stream); err != nil {
		return nil, err
	}
	if request.ID == "wait" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	body := []byte(input.Text)
	if request.ID == "forward" {
		body = []byte("before-forward")
	}
	for len(body) != 0 {
		size := min(len(body), sdk.MaxChunkBytes)
		if err := stream.Emit(ctx, "data", body[:size]); err != nil {
			return nil, err
		}
		body = body[size:]
	}
	if request.ID == "forward" {
		call.Params = request.Arguments
		result, err := forwardAccepted(ctx, call, stream)
		if err != nil {
			return nil, err
		}
		return hybrid.ToolContent(result)
	}
	result, err := hybrid.Describe(ctx, call, input.Text)
	if err != nil {
		return nil, err
	}
	return hybrid.ToolContent(result)
}

func finishAuth(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
	var request sdk.AuthFinishRequest
	if err := json.Unmarshal(call.Params, &request); err != nil {
		return nil, err
	}
	var input struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(request.Input, &input) != nil || (input.ID != "account-a" && input.ID != "account-b") {
		return nil, &sdk.Error{Code: "invalid_params", Message: "Use synthetic account-a or account-b."}
	}
	credentials, err := json.Marshal(map[string]string{"subject": input.ID, "fixture": "offline-only"})
	return sdk.AuthResult{ID: input.ID, Name: input.ID, Credentials: credentials}, err
}
