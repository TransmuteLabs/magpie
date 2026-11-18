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

type config struct {
	Trace   string `json:"trace"`
	Started string `json:"started"`
	Release string `json:"release"`
}

func main() {
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		panic(err)
	}
	var manifest sdk.Manifest
	if json.Unmarshal(raw, &manifest) != nil {
		panic("invalid window fixture manifest")
	}
	service := slices.Contains(os.Args, "--service")
	if service {
		manifest, err = sdk.ServiceManifest(manifest, "helper")
		if err != nil {
			panic(err)
		}
	}
	start := func(ctx context.Context, current sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
		var options config
		if json.Unmarshal(current.Config.Values, &options) != nil {
			return sdk.ModuleReady{}, nil, errors.New("invalid window fixture config")
		}
		if service && options.Started != "" {
			if err := os.WriteFile(options.Started, []byte("helper-ready"), 0600); err != nil {
				return sdk.ModuleReady{}, nil, err
			}
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				if _, err := os.Stat(options.Release); err == nil {
					break
				}
				select {
				case <-ctx.Done():
					return sdk.ModuleReady{}, nil, ctx.Err()
				case <-tick.C:
				}
			}
		}
		return sdk.ModuleReady{State: options}, nil, nil
	}
	action := func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
		if call.Scope.Account != "" {
			var auth struct {
				Credential string `json:"credential"`
			}
			if json.Unmarshal(call.Auth, &auth) != nil || auth.Credential != "SECRET-"+call.Scope.Account {
				return nil, errors.New("window fixture received another account's credentials")
			}
		} else if len(call.Auth) != 0 {
			return nil, errors.New("root action received credentials")
		}
		if call.Action == "helper.write" {
			if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
				return nil, err
			}
		}
		current, _ := sdk.ModuleContextFrom(ctx)
		options := current.State.(config)
		if options.Trace != "" {
			file, err := os.OpenFile(options.Trace, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return nil, err
			}
			err = json.NewEncoder(file).Encode(map[string]any{"account": call.Scope.Account, "action": call.Action})
			file.Close()
			if err != nil {
				return nil, err
			}
		}
		return map[string]any{"account": call.Scope.Account, "generation": call.Scope.Generation, "component": current.Component}, nil
	}
	module := sdk.Module{Manifest: manifest, Start: start}
	if service {
		module.Actions = map[string]sdk.Handler{"helper.read": action, "helper.write": action}
	} else {
		module.Handlers = map[string]sdk.Handler{
			sdk.MethodProviderModels: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
				return sdk.ModelsResult{}, nil
			},
		}
		for _, method := range []string{sdk.MethodAuthStart, sdk.MethodAuthFinish, sdk.MethodAuthRefresh, sdk.MethodAuthLogout} {
			module.Handlers[method] = func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
				return nil, &sdk.Error{Code: "unsupported", Message: "Window fixture accounts are host-owned."}
			}
		}
	}
	if sdk.Serve(context.Background(), os.Stdin, os.Stdout, module) != nil {
		os.Exit(1)
	}
}
