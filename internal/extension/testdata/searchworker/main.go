package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type config struct {
	Mode     string `json:"mode"`
	Trace    string `json:"trace"`
	Started  string `json:"started"`
	Release  string `json:"release"`
	Endpoint string `json:"endpoint"`
}

type credential struct {
	Token string `json:"token"`
	Mode  string `json:"mode"`
}

func main() {
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		panic(err)
	}
	var manifest sdk.Manifest
	if json.Unmarshal(raw, &manifest) != nil {
		panic("invalid search fixture manifest")
	}
	module := sdk.Module{
		Manifest: manifest,
		Start: func(_ context.Context, current sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
			var options config
			if json.Unmarshal(current.Config.Values, &options) != nil {
				return sdk.ModuleReady{}, nil, errors.New("invalid fixture configuration")
			}
			return sdk.ModuleReady{State: options}, nil, nil
		},
		Actions: map[string]sdk.Handler{"search-read": func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
			var request sdk.SearchRequest
			if json.Unmarshal(call.Params, &request) != nil || request.Validate() != nil || request.Source != "specstream" ||
				call.Scope.Operation != "" || call.Scope.Session != "" {
				return nil, errors.New("invalid search authority")
			}
			var auth credential
			_ = json.Unmarshal(call.Auth, &auth)
			if call.Scope.Account == "" && len(call.Auth) != 0 || call.Scope.Account != "" && auth.Token == "" {
				return nil, errors.New("wrong selected search credentials")
			}
			current, _ := sdk.ModuleContextFrom(ctx)
			options := current.State.(config)
			target := options.Endpoint
			if target == "" {
				target = "https://search.example.invalid/search"
			}
			var proxy sdk.NetworkProxyResult
			if err := stream.HostCall(ctx, sdk.MethodNetworkProxy, sdk.NetworkProxyRequest{URL: target}, &proxy); err != nil {
				return nil, err
			}
			if options.Trace != "" {
				file, err := os.OpenFile(options.Trace, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					return nil, err
				}
				err = json.NewEncoder(file).Encode(map[string]any{"version": manifest.Version, "request": request,
					"scope": call.Scope, "proxy": proxy.URL, "hasAuth": len(call.Auth) != 0})
				closeErr := file.Close()
				if err != nil || closeErr != nil {
					return nil, errors.New("fixture trace failed")
				}
			}
			if options.Mode == "hold" {
				if err := os.WriteFile(options.Started, []byte("searching"), 0600); err != nil {
					return nil, err
				}
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for {
					if _, err := os.Stat(options.Release); err == nil {
						break
					}
					select {
					case <-ctx.Done():
						_ = os.WriteFile(options.Started+".canceled", []byte("canceled"), 0600)
						return nil, ctx.Err()
					case <-tick.C:
					}
				}
			}
			mode := options.Mode
			if auth.Mode != "" {
				mode = auth.Mode
			}
			switch mode {
			case "error":
				return nil, errors.New("fixture failure " + auth.Token)
			case "invalid":
				return map[string]any{"pages": []any{}, "credentials": auth.Token}, nil
			case "refused":
				return sdk.SearchResult{Pages: []sdk.SearchPage{}, Status: 429, Error: "slow down " + auth.Token, RetryAfter: "30"}, nil
			case "state-write":
				var result sdk.StateCASResult
				if stream.HostCall(ctx, sdk.MethodStateCAS, sdk.StateCASRequest{Key: "search.must-not-write",
					Version: "0", Value: json.RawMessage(`{"written":true}`)}, &result) == nil {
					return nil, errors.New("read action acquired write authority")
				}
			}
			if options.Endpoint != "" {
				transport := &http.Transport{}
				defer transport.CloseIdleConnections()
				if proxy.URL != "" {
					address, err := url.Parse(proxy.URL)
					if err != nil {
						return nil, err
					}
					transport.Proxy = http.ProxyURL(address)
				}
				upstream, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
				if err != nil {
					return nil, err
				}
				upstream.Header.Set("Authorization", "Bearer "+auth.Token)
				response, err := (&http.Client{Transport: transport}).Do(upstream)
				if err != nil {
					return nil, err
				}
				_, readErr := io.Copy(io.Discard, response.Body)
				closeErr := response.Body.Close()
				if response.StatusCode != 200 || readErr != nil || closeErr != nil {
					return nil, errors.New("fixture upstream failed")
				}
			}
			return sdk.SearchResult{Pages: []sdk.SearchPage{{Title: "SpecStream <b>fixture</b>", URL: "https://example.invalid/found",
				Text: "ordinary   search\n" + manifest.Version, Age: "2026-11-14"}}}, nil
		}},
	}
	if manifest.Contract.Has(sdk.AccountAuth) {
		unsupported := func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
			return nil, errors.New("fixture method unavailable")
		}
		module.Handlers = map[string]sdk.Handler{sdk.MethodAuthStart: unsupported,
			sdk.MethodAuthFinish: unsupported, sdk.MethodAuthLogout: unsupported,
			sdk.MethodAuthRefresh: func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
				var auth credential
				if json.Unmarshal(call.Auth, &auth) != nil || auth.Mode == "refresh-failed" {
					return nil, errors.New("fixture refresh failed")
				}
				return sdk.AuthResult{ID: call.Scope.Account, Credentials: json.RawMessage(`{"token":"PRIVATE-REFRESHED"}`),
					ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, nil
			}}
		if manifest.Contract.Has(sdk.OutboundExecutor) {
			module.Handlers[sdk.MethodProviderExecute] = unsupported
		}
		if manifest.Contract.Has(sdk.ModelDiscovery) {
			module.Handlers[sdk.MethodProviderModels] = func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
				if call.Scope.Provider != "inference" {
					return nil, errors.New("model discovery reached an account-only service")
				}
				return sdk.ModelsResult{Models: []sdk.Model{{ID: "sibling-model"}}}, nil
			}
		}
	}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil {
		os.Exit(1)
	}
}
