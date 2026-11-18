package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func execute(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var request sdk.ExecuteRequest
	var auth struct{ Upstream string }
	if err := json.Unmarshal(call.Params, &request); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(call.Auth, &auth); err != nil {
		return nil, err
	}
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, auth.Upstream+"/v1/chat/completions", bytes.NewReader(request.Body))
	if err != nil {
		return nil, err
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("X-Fixture-Account", call.Scope.Account)
	if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
		return nil, err
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(upstream)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: response.StatusCode, Headers: response.Header}); err != nil {
		return nil, err
	}
	buffer := make([]byte, 8192)
	for {
		count, readErr := response.Body.Read(buffer)
		if count > 0 {
			if err := stream.Emit(ctx, "data", buffer[:count]); err != nil {
				return nil, err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return sdk.ExecuteResult{Status: response.StatusCode, Outcome: "completed"}, nil
}

func main() {
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		os.Exit(1)
	}
	var manifest sdk.Manifest
	if json.Unmarshal(raw, &manifest) != nil {
		os.Exit(1)
	}
	module := sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{sdk.MethodProviderExecute: execute}}
	for _, method := range []string{sdk.MethodAuthStart, sdk.MethodAuthFinish, sdk.MethodAuthRefresh, sdk.MethodAuthLogout} {
		module.Handlers[method] = func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
			return nil, &sdk.Error{Code: "unsupported", Message: "The fixture uses preselected synthetic accounts"}
		}
	}
	if sdk.Serve(context.Background(), os.Stdin, os.Stdout, module) != nil {
		os.Exit(1)
	}
}
