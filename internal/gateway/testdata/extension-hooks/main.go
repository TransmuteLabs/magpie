package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type configuration struct {
	Marker      string `json:"marker"`
	RequestURL  string `json:"requestURL"`
	ResponseURL string `json:"responseURL"`
	SignInURL   string `json:"signInURL"`
	Protected   bool   `json:"protected"`
}

type delivery struct {
	Actor    string               `json:"actor"`
	Request  *sdk.GatewayRequest  `json:"request,omitempty"`
	Response *sdk.GatewayResponse `json:"response,omitempty"`
	SignIn   *sdk.GatewaySignIn   `json:"signIn,omitempty"`
	Scope    sdk.Scope            `json:"scope"`
	Auth     json.RawMessage      `json:"auth"`
}

var client = &http.Client{Transport: &http.Transport{Proxy: nil}}

func options(ctx context.Context) (configuration, error) {
	module, ok := sdk.ModuleContextFrom(ctx)
	var config configuration
	if !ok || json.Unmarshal(module.Config.Values, &config) != nil {
		return config, errors.New("fixture configuration unavailable")
	}
	return config, nil
}

func post(ctx context.Context, target string, body delivery) error {
	if target == "" {
		return nil
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, response.Body)
	return err
}

func request(ctx context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
	config, err := options(ctx)
	if err != nil {
		return nil, err
	}
	var input sdk.GatewayRequest
	if err := json.Unmarshal(call.Params, &input); err != nil {
		return nil, err
	}
	if err := post(ctx, config.RequestURL, delivery{Actor: "sdk", Request: &input, Scope: call.Scope, Auth: call.Auth}); err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(input.Body, &body); err != nil {
		return nil, err
	}
	messages, _ := body["messages"].([]any)
	body["messages"] = append(messages, map[string]string{"role": "system", "content": "sdk recall: " + config.Marker})
	if config.Protected {
		body["model"] = "unauthorized-model"
	}
	accepted, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	state, _ := json.Marshal(map[string]string{"marker": config.Marker, "query": input.Context.Query})
	return sdk.GatewayRequestResult{Body: accepted, State: state}, nil
}

func response(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	config, err := options(ctx)
	if err != nil {
		return nil, err
	}
	var input sdk.GatewayResponse
	if err := json.Unmarshal(call.Params, &input); err != nil {
		return nil, err
	}
	if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
		return nil, err
	}
	return nil, post(ctx, config.ResponseURL, delivery{Actor: "sdk", Response: &input, Scope: call.Scope, Auth: call.Auth})
}

func signIn(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	config, err := options(ctx)
	if err != nil {
		return nil, err
	}
	var input sdk.GatewaySignIn
	if err := json.Unmarshal(call.Params, &input); err != nil {
		return nil, err
	}
	if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
		return nil, err
	}
	return nil, post(ctx, config.SignInURL, delivery{Actor: "sdk", SignIn: &input, Scope: call.Scope, Auth: call.Auth})
}

func main() {
	data, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		os.Exit(1)
	}
	var manifest sdk.Manifest
	if json.Unmarshal(data, &manifest) != nil || manifest.Hooks == nil {
		os.Exit(1)
	}
	handlers := map[string]sdk.Handler{}
	if manifest.Hooks.Request {
		handlers[sdk.MethodGatewayRequest] = request
	}
	if manifest.Hooks.Response {
		handlers[sdk.MethodGatewayResponse] = response
	}
	if manifest.Hooks.SignIn {
		handlers[sdk.MethodGatewaySignIn] = signIn
	}
	err = sdk.Serve(context.Background(), os.Stdin, os.Stdout, sdk.Module{Manifest: manifest, Handlers: handlers})
	client.CloseIdleConnections()
	if err != nil {
		os.Exit(1)
	}
}
