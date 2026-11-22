package authfixture

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

type Config struct {
	Trace          string `json:"trace"`
	Started        string `json:"started"`
	Release        string `json:"release"`
	Fail           bool   `json:"fail"`
	RefreshStarted string `json:"refreshStarted"`
	RefreshRelease string `json:"refreshRelease"`
	RefreshOutcome string `json:"refreshOutcome"`
	ImportProxy    string `json:"importProxy"`
	QueueRelease   string `json:"queueRelease"`
	ExecuteRelease string `json:"executeRelease"`
}

func Module() sdk.Module {
	manifest := Manifest()
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err == nil && json.Unmarshal(raw, &manifest) != nil {
		panic("invalid auth fixture manifest")
	}
	record := func(ctx context.Context, method string, call sdk.Call) (Config, error) {
		current, found := sdk.ModuleContextFrom(ctx)
		if !found {
			return Config{}, errors.New("missing fixture context")
		}
		var config Config
		if json.Unmarshal(current.Config.Values, &config) != nil {
			return config, errors.New("invalid fixture config")
		}
		if config.Trace != "" {
			file, err := os.OpenFile(config.Trace, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
			if err != nil {
				return config, err
			}
			err = json.NewEncoder(file).Encode(map[string]any{"method": method, "scope": call.Scope, "pid": os.Getpid()})
			file.Close()
			if err != nil {
				return config, err
			}
		}
		return config, nil
	}
	module := sdk.Module{
		Manifest: manifest,
		Start: func(context.Context, sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
			return sdk.ModuleReady{}, nil, nil
		},
		Handlers: map[string]sdk.Handler{
			sdk.MethodProviderModels: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
				return sdk.ModelsResult{}, nil
			},
			sdk.MethodAuthLogout: func(ctx context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
				_, err := record(ctx, "logout", call)
				return nil, err
			},
			sdk.MethodAuthRefresh: func(ctx context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
				config, err := record(ctx, "refresh", call)
				if err != nil {
					return nil, err
				}
				if config.RefreshStarted != "" {
					if err := os.WriteFile(config.RefreshStarted, []byte("started"), 0o600); err != nil {
						return nil, err
					}
				}
				for config.RefreshRelease != "" {
					if _, err := os.Stat(config.RefreshRelease); err == nil {
						break
					}
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(time.Millisecond):
					}
				}
				if config.RefreshOutcome != "" {
					return nil, &sdk.Error{Code: "fixture_refused", Message: "Fixture did not rotate the credential", Outcome: config.RefreshOutcome}
				}
				return sdk.AuthResult{ID: call.Scope.Account, Credentials: json.RawMessage(`{"token":"PRIVATE-REFRESHED"}`)}, nil
			},
			sdk.MethodAuthStart: func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
				config, err := record(ctx, "start", call)
				if err != nil {
					return nil, err
				}
				var request sdk.AuthStartRequest
				if json.Unmarshal(call.Params, &request) == nil && request.Method == "saved-signin" {
					var input struct{ Record string }
					if json.Unmarshal(request.Input, &input) != nil {
						return nil, errors.New("expected the selected synthetic saved sign-in")
					}
					continuation := `{"private":"PRIVATE-IMPORT"}`
					switch input.Record {
					case `{"type":"api","key":"PRIVATE-SAVED"}`:
					case `{"type":"api","key":"PRIVATE-ROTATED-SAVED"}`:
						continuation = `{"private":"PRIVATE-IMPORT-ROTATED"}`
					case `{"type":"api","key":"PRIVATE-OTHER"}`:
						continuation = `{"private":"PRIVATE-IMPORT-OTHER"}`
					default:
						return nil, errors.New("expected the selected synthetic saved sign-in")
					}
					if config.ImportProxy != "" {
						var network sdk.NetworkProxyResult
						if err := stream.HostCall(ctx, sdk.MethodNetworkProxy, sdk.NetworkProxyRequest{URL: "https://fixture.invalid"}, &network); err != nil ||
							network.URL != config.ImportProxy {
							return nil, errors.New("saved sign-in did not resolve through the selected source proxy")
						}
					}
					var state sdk.StateCASResult
					if err := stream.HostCall(ctx, sdk.MethodStateCAS, sdk.StateCASRequest{
						Key: "fixture.saved-sign-in", Version: "0", Value: json.RawMessage(input.Record),
					}, &state); err != nil || !state.Swapped {
						return nil, errors.New("expected empty temporary sign-in state")
					}
					return sdk.AuthStartResult{Continuation: json.RawMessage(continuation)}, nil
				}
				return sdk.AuthStartResult{
					URL: "https://fixture.invalid/verify", UserCode: "SAFE-CODE", PollAfterMs: 1000,
					Continuation: json.RawMessage(`{"private":"PRIVATE-CONTINUATION"}`),
				}, nil
			},
			sdk.MethodAuthFinish: func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
				config, err := record(ctx, "finish", call)
				if err != nil {
					return nil, err
				}
				var request sdk.AuthFinishRequest
				if json.Unmarshal(call.Params, &request) != nil {
					return nil, errors.New("missing private fixture continuation")
				}
				result := sdk.AuthResult{ID: "chosen", Name: "reader@example.test", Credentials: json.RawMessage(`{"token":"PRIVATE-CREDENTIAL"}`)}
				switch string(request.Continuation) {
				case `{"private":"PRIVATE-IMPORT"}`, `{"private":"PRIVATE-IMPORT-OTHER"}`, `{"private":"PRIVATE-IMPORT-ROTATED"}`:
					record := `{"type":"api","key":"PRIVATE-SAVED"}`
					if string(request.Continuation) == `{"private":"PRIVATE-IMPORT-OTHER"}` {
						record = `{"type":"api","key":"PRIVATE-OTHER"}`
						result = sdk.AuthResult{ID: "other", Name: "other@example.test", Credentials: json.RawMessage(`{"token":"PRIVATE-OTHER"}`)}
					} else if string(request.Continuation) == `{"private":"PRIVATE-IMPORT-ROTATED"}` {
						record = `{"type":"api","key":"PRIVATE-ROTATED-SAVED"}`
						result.Credentials = json.RawMessage(`{"token":"PRIVATE-ROTATED"}`)
					}
					var state sdk.StateValue
					if err := stream.HostCall(ctx, sdk.MethodStateGet, sdk.StateGetRequest{Key: "fixture.saved-sign-in"}, &state); err != nil ||
						!state.Found || state.Version != "1" || string(state.Value) != record {
						return nil, errors.New("finish lost temporary sign-in state")
					}
				case `{"private":"PRIVATE-CONTINUATION"}`:
				default:
					return nil, errors.New("missing private fixture continuation")
				}
				if config.Started != "" {
					if err := os.WriteFile(config.Started, []byte("started"), 0o600); err != nil {
						return nil, err
					}
				}
				for config.Release != "" {
					if _, err := os.Stat(config.Release); err == nil {
						break
					}
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(time.Millisecond):
					}
				}
				if config.Fail {
					return nil, &sdk.Error{Code: "vendor", Message: "PRIVATE-CONTINUATION PRIVATE-CREDENTIAL", Outcome: "uncertain"}
				}
				return result, nil
			},
		},
	}
	if manifest.Contract.Has(sdk.OutboundExecutor) {
		module.Handlers[sdk.MethodProviderExecute] = func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
			config, err := record(ctx, "execute", call)
			if err != nil {
				return nil, err
			}
			if config.ExecuteRelease != "" {
				select {
				case <-ctx.Done():
				case <-time.After(10 * time.Second):
					return nil, errors.New("fixture request was not canceled")
				}
				if _, err := record(ctx, "execute-canceled", call); err != nil {
					return nil, err
				}
				deadline := time.After(5 * time.Second)
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					if _, err := os.Stat(config.ExecuteRelease); err == nil {
						return nil, ctx.Err()
					}
					select {
					case <-deadline:
						return nil, errors.New("fixture execution was not released")
					case <-ticker.C:
					}
				}
			}
			if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: http.StatusOK,
				Headers: http.Header{"Content-Type": {"application/json"}}}); err != nil {
				return nil, err
			}
			if err := stream.Emit(ctx, "data", []byte(`{"id":"fixture","choices":[{"message":{"role":"assistant","content":"SDK executed"},"finish_reason":"stop"}]}`)); err != nil {
				return nil, err
			}
			return sdk.ExecuteResult{Status: http.StatusOK, Outcome: "completed"}, nil
		}
	}
	if manifest.Contract.Has(sdk.UsageReporter) {
		module.Handlers[sdk.MethodProviderUsage] = func(ctx context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			var request sdk.UsageRequest
			if err := json.Unmarshal(call.Params, &request); err != nil {
				return nil, err
			}
			var token string
			var used float64
			switch call.Scope.Account {
			case "chosen":
				token, used = `{"token":"PRIVATE-CREDENTIAL"}`, 17
			case "other":
				token, used = `{"token":"PRIVATE-OTHER"}`, 23
			}
			if token == "" || string(call.Auth) != token {
				return nil, errors.New("usage lacks the selected account")
			}
			return sdk.UsageResult{Revision: request.Revision, Windows: []sdk.UsageWindow{{Name: "Weekly", Used: used}}}, nil
		}
	}
	if manifest.Contract.Has(sdk.ProviderSignIn) {
		module.Handlers[sdk.MethodProviderModels] = func(ctx context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			if call.Scope.Session == "hold-queue" {
				config, err := record(ctx, "hold-queue", call)
				if err != nil {
					return nil, err
				}
				for config.QueueRelease != "" {
					if _, err := os.Stat(config.QueueRelease); err == nil {
						break
					}
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(time.Millisecond):
					}
				}
			}
			return sdk.ModelsResult{Models: manifest.Providers[0].Models}, nil
		}
	}
	return module
}

func Manifest() sdk.Manifest {
	return sdk.Manifest{
		Schema: 1, ID: "window-auth", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 5},
			Capabilities: []sdk.Capability{sdk.ModuleRPC, sdk.AccountAuth, sdk.ModelDiscovery}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Vendor", Protocols: []protocolext.Protocol{protocolext.Chat}}},
		Window:    &sdk.WindowDescriptor{Metadata: "package.json"},
		AuthBindings: []sdk.WindowAuthBindingDescriptor{{
			ID: "signin", From: "window:page", Provider: "vendor", Reauthorize: true,
			Methods: []sdk.WindowAuthMethod{{ID: "browser", Mode: "poll"}, {ID: "device", Mode: "poll"}},
		}},
	}
}
