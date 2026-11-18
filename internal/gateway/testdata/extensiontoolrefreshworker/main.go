package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func refresh(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var auth struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(call.Auth, &auth) != nil || auth.URL == "" {
		return nil, errors.New("missing synthetic refresh endpoint")
	}
	body, _ := json.Marshal(call)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, auth.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &sdk.Error{Code: "fixture", Message: "refresh refused", Outcome: "not-started"}
	}
	var result sdk.AuthResult
	err = json.NewDecoder(response.Body).Decode(&result)
	return result, err
}

func tool(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var request sdk.ToolRequest
	var arguments struct {
		Replace bool `json:"fixture-replace"`
		Event   bool `json:"fixture-after-event"`
	}
	if json.Unmarshal(call.Params, &request) != nil || json.Unmarshal(request.Arguments, &arguments) != nil {
		return nil, errors.New("invalid fixture arguments")
	}
	if strings.Contains(string(call.Params), "fixture-accepted") {
		if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
			return nil, err
		}
	}
	var auth struct {
		URL string `json:"url"`
	}
	var replacement *sdk.AccountReplaceResult
	callbacksDenied := false
	if arguments.Replace {
		replacement = &sdk.AccountReplaceResult{}
		if err := stream.HostCall(ctx, sdk.MethodAccountReplace,
			sdk.AccountReplaceRequest{Credentials: json.RawMessage(`{"token":"synthetic-replaced"}`)}, replacement); err != nil {
			return nil, err
		}
		var proxy sdk.NetworkProxyResult
		if err := stream.HostCall(ctx, sdk.MethodNetworkProxy,
			sdk.NetworkProxyRequest{URL: "https://203.0.113.40/endpoint"}, &proxy); err == nil {
			return nil, errors.New("replacement renewed original callback authority")
		}
		var repeated sdk.AccountReplaceResult
		if err := stream.HostCall(ctx, sdk.MethodAccountReplace,
			sdk.AccountReplaceRequest{Credentials: json.RawMessage(`{"token":"must-not-save"}`)}, &repeated); err == nil {
			return nil, errors.New("replacement permitted another original-scope CAS")
		}
		callbacksDenied = true
	}
	if json.Unmarshal(call.Auth, &auth) == nil && auth.URL != "" {
		observation, err := json.Marshal(call)
		if err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, auth.URL+"/tool", bytes.NewReader(observation))
		if err != nil {
			return nil, err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return nil, err
		}
		response.Body.Close()
	}
	if arguments.Event {
		if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
			return nil, err
		}
	}
	if strings.Contains(string(call.Params), "fixture-401") {
		return nil, sdk.NewHTTPError(http.StatusUnauthorized, "fixture request refused")
	}
	if strings.Contains(string(call.Params), "fixture-uncertain") {
		return nil, &sdk.Error{Code: "fixture", Message: "fixture uncertain result", Outcome: "uncertain"}
	}
	body, _ := json.Marshal(struct {
		Scope           sdk.Scope                 `json:"scope"`
		Auth            json.RawMessage           `json:"auth"`
		Params          json.RawMessage           `json:"params"`
		Replacement     *sdk.AccountReplaceResult `json:"replacement,omitempty"`
		CallbacksDenied bool                      `json:"callbacksDenied,omitempty"`
	}{call.Scope, call.Auth, call.Params, replacement, callbacksDenied})
	return sdk.ToolResult{Content: body}, nil
}

func main() {
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		panic(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		panic(err)
	}
	unsupported := func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
		return nil, errors.New("unexpected fixture method")
	}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, sdk.Module{
		Manifest: manifest,
		Handlers: map[string]sdk.Handler{
			sdk.MethodToolCall: tool, sdk.MethodAuthRefresh: refresh,
			sdk.MethodAuthStart: unsupported, sdk.MethodAuthFinish: unsupported, sdk.MethodAuthLogout: unsupported,
		},
	}); err != nil {
		panic(err)
	}
}
