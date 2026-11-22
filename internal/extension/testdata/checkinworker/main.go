package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type options struct {
	Trace   string `json:"trace"`
	Mode    string `json:"mode"`
	Started string `json:"started"`
	Release string `json:"release"`
}

func record(path string, value any) error {
	if path == "" {
		return nil
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(value)
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
	if slices.Contains(os.Args, "--checkin-helper") {
		manifest, err = sdk.ServiceManifest(manifest, "helper")
		if err != nil {
			panic(err)
		}
	}
	module := sdk.Module{
		Manifest: manifest,
		Start: func(ctx context.Context, current sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
			var config options
			if err := json.Unmarshal(current.Config.Values, &config); err != nil {
				return sdk.ModuleReady{}, nil, err
			}
			dispose := func() {
				_ = record(config.Trace, map[string]any{"event": "stop", "instance": current.Instance})
			}
			if err := record(config.Trace, map[string]any{"event": "start", "instance": current.Instance, "config": current.Config.Version}); err != nil {
				return sdk.ModuleReady{}, dispose, err
			}
			if config.Mode == "startup-error" {
				return sdk.ModuleReady{}, dispose, errors.New("fixture startup refusal")
			}
			if config.Mode == "hold-service" && manifest.Component != nil {
				if err := os.WriteFile(config.Started, []byte("held"), 0600); err != nil {
					return sdk.ModuleReady{}, dispose, err
				}
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					if _, err := os.Stat(config.Release); err == nil {
						break
					}
					select {
					case <-ctx.Done():
						return sdk.ModuleReady{}, dispose, ctx.Err()
					case <-ticker.C:
					}
				}
			}
			return sdk.ModuleReady{State: config}, dispose, nil
		},
		Actions: map[string]sdk.Handler{
			"daily.claim": func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
				current, _ := sdk.ModuleContextFrom(ctx)
				config := current.State.(options)
				var auth struct {
					Token string `json:"token"`
					Mode  string `json:"mode"`
					Until string `json:"until"`
				}
				if json.Unmarshal(call.Auth, &auth) != nil || auth.Token != call.Scope.Account ||
					call.Scope.Provider != "vendor" || call.Scope.Operation == "" {
					return nil, errors.New("wrong selected check-in authority")
				}
				if auth.Mode == "not-started" {
					if err := record(config.Trace, map[string]any{"event": "refused", "scope": call.Scope}); err != nil {
						return nil, err
					}
					return nil, &sdk.Error{Code: "held", Message: "Definitely not started", Outcome: "not-started"}
				}
				if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
					return nil, err
				}
				if err := record(config.Trace, map[string]any{
					"event": "claim", "scope": call.Scope, "request": json.RawMessage(call.Params),
				}); err != nil {
					return nil, err
				}
				switch auth.Mode {
				case "lost":
					os.Exit(3)
				case "invalid":
					return map[string]any{"outcome": "invented"}, nil
				case "error":
					return nil, errors.New("accepted claim failed")
				case "failed", "rate-limited":
					return map[string]any{"outcome": auth.Mode, "until": auth.Until, "message": "fixture vendor pause"}, nil
				case "done", "inactive":
					return map[string]any{"outcome": auth.Mode}, nil
				}
				return map[string]any{"outcome": "claimed", "credit": 12.5, "streak": 3, "message": "fixture grant"}, nil
			},
		},
		Handlers: map[string]sdk.Handler{
			sdk.MethodProviderExecute: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
				return sdk.ExecuteResult{Status: 200, Outcome: "completed"}, nil
			},
			sdk.MethodAuthStart:  func(context.Context, sdk.Call, *sdk.Stream) (any, error) { return sdk.AuthResult{}, nil },
			sdk.MethodAuthFinish: func(context.Context, sdk.Call, *sdk.Stream) (any, error) { return sdk.AuthResult{}, nil },
			sdk.MethodAuthRefresh: func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
				return sdk.AuthResult{Credentials: call.Auth}, nil
			},
			sdk.MethodAuthLogout: func(context.Context, sdk.Call, *sdk.Stream) (any, error) { return sdk.AuthResult{}, nil },
		},
	}
	if action, found := manifest.ResolveAction("daily.claim"); found && action.Component == "helper" && manifest.Component == nil {
		module.Actions = nil
	}
	for method := range module.Handlers {
		if !slices.Contains(sdk.RequiredMethods(manifest), method) {
			delete(module.Handlers, method)
		}
	}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil {
		os.Exit(1)
	}
}
