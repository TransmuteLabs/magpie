package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type configuration struct {
	Trace     string `json:"trace"`
	Mode      string `json:"mode"`
	SkipFirst bool   `json:"skipFirst"`
	Started   string `json:"started"`
	Release   string `json:"release"`
}

func main() {
	var manifest sdk.Manifest
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err != nil || json.Unmarshal(raw, &manifest) != nil {
		os.Exit(2)
	}
	var lock sync.Mutex
	handlers := map[string]sdk.Handler{}
	for _, method := range manifest.Middleware.Methods() {
		handlers[method] = func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
			current, _ := sdk.ModuleContextFrom(ctx)
			options := current.State.(configuration)
			var request sdk.MiddlewareCall
			if err := json.Unmarshal(call.Params, &request); err != nil {
				return nil, err
			}
			state := map[string]any{}
			_ = json.Unmarshal(request.State, &state)
			if state == nil {
				state = map[string]any{}
			}
			denied := false
			if options.Mode == "write" {
				var result sdk.StateCASResult
				denied = stream.HostCall(ctx, sdk.MethodStateCAS,
					sdk.StateCASRequest{Key: "middleware.must-not-write", Value: json.RawMessage(`{"bad":true}`)}, &result) != nil
			}
			if options.Trace != "" {
				record, _ := json.Marshal(map[string]any{
					"method": method, "request": request, "scope": call.Scope,
					"hasAuth": len(call.Auth) > 0 && string(call.Auth) != "null", "writeDenied": denied,
				})
				lock.Lock()
				file, err := os.OpenFile(options.Trace, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
				if err == nil {
					_, err = file.Write(append(record, '\n'))
					_ = file.Close()
				}
				lock.Unlock()
				if err != nil {
					return nil, err
				}
			}
			if options.Mode == "error" || options.Mode == "request-error" && method == sdk.MethodMiddlewareRequest {
				return nil, errors.New("PRIVATE-MIDDLEWARE-DETAILS")
			}
			if options.Mode == "emit" {
				_ = stream.Emit(ctx, "accepted", map[string]string{"operationId": "forged"})
			}
			if options.Mode == "hold" && method == sdk.MethodMiddlewareRequest {
				_ = os.WriteFile(options.Started, []byte("started"), 0600)
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for {
					if _, err := os.Stat(options.Release); err == nil {
						break
					}
					select {
					case <-ctx.Done():
						_ = os.WriteFile(options.Started+".cancelled", []byte("cancelled"), 0600)
						return nil, ctx.Err()
					case <-ticker.C:
					}
				}
			}
			result := sdk.MiddlewareResult{}
			var body map[string]any
			_ = json.Unmarshal(request.Body, &body)
			switch method {
			case sdk.MethodMiddlewareRequest:
				if options.Mode == "reject" {
					return sdk.MiddlewareResult{Reject: &sdk.MiddlewareRejection{Status: 403, Message: "fixture denied request"}}, nil
				}
				state["request"] = "request-seen"
				body["temperature"] = 0.25
				if options.Mode == "protected" {
					body["model"] = "forbidden/m1"
					state["unsafe"] = true
				}
				if options.Mode == "oversize-state" {
					result.State, _ = json.Marshal(strings.Repeat("x", sdk.MaxGatewayStateBytes))
				}
			case sdk.MethodMiddlewareAccount:
				if options.SkipFirst && state["skipped"] == nil {
					state["skipped"] = request.Context.Account.Ref
					result.Skip = true
				}
			case sdk.MethodMiddlewareProvider:
				if request.Context.Provider.ID == "first" {
					body["temperature"] = 0.5
				}
			case sdk.MethodMiddlewareEvent:
				body["sdkState"] = state["request"]
				body["sdkAccount"] = request.Context.Account != nil
				if options.Mode == "drop" {
					return sdk.MiddlewareResult{Drop: true}, nil
				}
			case sdk.MethodMiddlewareResponse:
				body["sdkState"] = state["request"]
				body["sdkAccount"] = request.Context.Account != nil
			}
			if body != nil {
				result.Body, _ = json.Marshal(body)
			}
			if len(result.State) == 0 {
				result.State, _ = json.Marshal(state)
			}
			return result, nil
		}
	}
	module := sdk.Module{Manifest: manifest, Handlers: handlers,
		Start: func(_ context.Context, module sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
			var options configuration
			err := json.Unmarshal(module.Config.Values, &options)
			return sdk.ModuleReady{State: options}, nil, err
		}}
	if sdk.Serve(context.Background(), os.Stdin, os.Stdout, module) != nil {
		os.Exit(1)
	}
}
