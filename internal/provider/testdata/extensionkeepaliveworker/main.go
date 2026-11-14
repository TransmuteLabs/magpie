package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func refresh(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var auth struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(call.Auth, &auth) != nil || auth.URL == "" {
		return nil, errors.New("missing synthetic account endpoint")
	}
	var proxy sdk.NetworkProxyResult
	if err := stream.HostCall(ctx, sdk.MethodNetworkProxy, sdk.NetworkProxyRequest{URL: auth.URL}, &proxy); err != nil {
		return nil, err
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	if proxy.URL != "" {
		selected, err := url.Parse(proxy.URL)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(selected)
	}
	body, err := json.Marshal(struct {
		Scope sdk.Scope       `json:"scope"`
		Auth  json.RawMessage `json:"auth"`
		Proxy string          `json:"proxy"`
	}{call.Scope, call.Auth, proxy.URL})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, auth.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &sdk.Error{Code: "fixture", Message: "Synthetic renewal failed", Outcome: "not-started"}
	}
	var result sdk.AuthResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
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
		return nil, &sdk.Error{Code: "fixture", Message: "Idle renewal must not execute business work", Outcome: "not-started"}
	}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, sdk.Module{
		Manifest: manifest,
		Handlers: map[string]sdk.Handler{
			sdk.MethodAuthRefresh: refresh, sdk.MethodAuthStart: unsupported,
			sdk.MethodAuthFinish: unsupported, sdk.MethodAuthLogout: unsupported,
			sdk.MethodProviderExecute: unsupported,
		},
	}); err != nil {
		panic(err)
	}
}
