package authfixture

import (
	"context"
	"encoding/json"
	"errors"
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
	unsupported := func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
		return nil, &sdk.Error{Code: "unsupported", Message: "Not used by this fixture"}
	}
	return sdk.Module{
		Manifest: manifest,
		Start: func(context.Context, sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
			return sdk.ModuleReady{}, nil, nil
		},
		Handlers: map[string]sdk.Handler{
			sdk.MethodProviderModels: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
				return sdk.ModelsResult{}, nil
			},
			sdk.MethodAuthLogout: unsupported,
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
				return sdk.AuthResult{ID: call.Scope.Account, Credentials: json.RawMessage(`{"token":"PRIVATE-REFRESHED"}`)}, nil
			},
			sdk.MethodAuthStart: func(ctx context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
				if _, err := record(ctx, "start", call); err != nil {
					return nil, err
				}
				return sdk.AuthStartResult{
					URL: "https://fixture.invalid/verify", UserCode: "SAFE-CODE", PollAfterMs: 1000,
					Continuation: json.RawMessage(`{"private":"PRIVATE-CONTINUATION"}`),
				}, nil
			},
			sdk.MethodAuthFinish: func(ctx context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
				config, err := record(ctx, "finish", call)
				if err != nil {
					return nil, err
				}
				var request sdk.AuthFinishRequest
				if json.Unmarshal(call.Params, &request) != nil || string(request.Continuation) != `{"private":"PRIVATE-CONTINUATION"}` {
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
				return sdk.AuthResult{ID: "chosen", Credentials: json.RawMessage(`{"token":"PRIVATE-CREDENTIAL"}`)}, nil
			},
		},
	}
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
