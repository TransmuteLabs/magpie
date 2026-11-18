package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"runtime"

	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/extension/native"
)

//go:embed magpie-plugin.json
var declaration []byte

func init() {
	var manifest sdk.Manifest
	if err := json.Unmarshal(declaration, &manifest); err != nil {
		panic(err)
	}
	switch runtime.GOOS {
	case "darwin":
		manifest.Entry = "plugin.dylib"
	case "windows":
		manifest.Entry = "plugin.dll"
	default:
		manifest.Entry = "plugin.so"
	}
	if err := native.Register(sdk.Module{
		Manifest: manifest,
		Handlers: map[string]sdk.Handler{sdk.MethodToolCall: tool},
	}); err != nil {
		panic(err)
	}
}

func main() {}

func tool(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var request sdk.ToolRequest
	if err := json.Unmarshal(call.Params, &request); err != nil {
		return nil, invalidRequest()
	}
	var arguments struct {
		Text  string          `json:"text"`
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(request.Arguments, &arguments); err != nil {
		return nil, invalidRequest()
	}
	switch request.ID {
	case "pid":
		return content(map[string]any{"pid": os.Getpid(), "text": arguments.Text})
	case "stream", "wait", "state":
		if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
			return nil, err
		}
	default:
		return nil, invalidRequest()
	}
	switch request.ID {
	case "stream":
		body := []byte(arguments.Text)
		for len(body) > 0 {
			size := min(len(body), sdk.MaxChunkBytes)
			if err := stream.Emit(ctx, "data", body[:size]); err != nil {
				return nil, err
			}
			body = body[size:]
		}
		if err := stream.Emit(ctx, "flush", nil); err != nil {
			return nil, err
		}
		return content(map[string]any{"pid": os.Getpid(), "text": arguments.Text})
	case "state":
		if arguments.Key == "" {
			arguments.Key = "echo"
		}
		var value sdk.StateValue
		if err := stream.HostCall(ctx, sdk.MethodStateGet, sdk.StateGetRequest{Key: arguments.Key}, &value); err != nil {
			return nil, err
		}
		if arguments.Value == nil {
			return content(value)
		}
		var result sdk.StateCASResult
		if err := stream.HostCall(ctx, sdk.MethodStateCAS, sdk.StateCASRequest{
			Key: arguments.Key, Version: value.Version, Value: arguments.Value,
		}, &result); err != nil {
			return nil, err
		}
		return content(result)
	case "wait":
		<-ctx.Done()
		return nil, ctx.Err()
	default:
		return nil, invalidRequest()
	}
}

func content(value any) (sdk.ToolResult, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return sdk.ToolResult{}, err
	}
	result, err := json.Marshal([]map[string]string{{"type": "text", "text": string(body)}})
	return sdk.ToolResult{Content: result}, err
}

func invalidRequest() error {
	return &sdk.Error{Code: "invalid_params", Message: "A declared native example tool and JSON arguments are required.", Outcome: "not-started"}
}
