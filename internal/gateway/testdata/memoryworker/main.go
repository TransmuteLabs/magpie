package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func main() {
	body, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		panic(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		panic(err)
	}
	handle := func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
		var tool sdk.ToolRequest
		if err := json.Unmarshal(call.Params, &tool); err != nil {
			return nil, err
		}
		var args struct {
			Search sdk.MemorySearchRequest `json:"search"`
			Times  int                     `json:"times"`
		}
		if err := json.Unmarshal(tool.Arguments, &args); err != nil {
			return nil, err
		}
		if args.Times < 1 || args.Times > 5 {
			return nil, errors.New("invalid fixture call count")
		}
		var result sdk.MemorySearchResult
		for range args.Times {
			if err := stream.HostCall(ctx, sdk.MethodMemorySearch, args.Search, &result); err != nil {
				return nil, err
			}
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		content, err := json.Marshal([]map[string]string{{"type": "text", "text": string(raw)}})
		return sdk.ToolResult{Content: content}, err
	}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, sdk.Module{
		Manifest: manifest, Handlers: map[string]sdk.Handler{sdk.MethodToolCall: handle},
	}); err != nil {
		panic(err)
	}
}
