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
	Mode         string `json:"mode"`
	Trace        string `json:"trace"`
	Started      string `json:"started"`
	Release      string `json:"release"`
	StartupDelay int    `json:"startupDelay"`
}

func main() {
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		panic(err)
	}
	var manifest sdk.Manifest
	if json.Unmarshal(raw, &manifest) != nil {
		panic("invalid routing fixture manifest")
	}
	if slices.Contains(os.Args, "--routing-service") {
		manifest, err = sdk.ServiceManifest(manifest, "backend")
		if err != nil {
			panic(err)
		}
	}
	module := sdk.Module{
		Manifest: manifest,
		Start: func(ctx context.Context, current sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
			var options config
			if json.Unmarshal(current.Config.Values, &options) != nil {
				return sdk.ModuleReady{}, nil, errors.New("invalid fixture configuration")
			}
			if options.Started != "" {
				if err := os.WriteFile(options.Started, []byte("started"), 0600); err != nil {
					return sdk.ModuleReady{}, nil, err
				}
			}
			if options.StartupDelay > 0 {
				timer := time.NewTimer(time.Duration(options.StartupDelay) * time.Millisecond)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return sdk.ModuleReady{}, nil, ctx.Err()
				case <-timer.C:
				}
			}
			return sdk.ModuleReady{State: options}, nil, nil
		},
		Actions: map[string]sdk.Handler{
			"rank": func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
				var request sdk.RoutingRequest
				if json.Unmarshal(call.Params, &request) != nil || request.Validate() != nil ||
					call.Scope.Provider != "" || call.Scope.Account != "" || len(call.Auth) != 0 {
					return nil, errors.New("routing authority or input changed")
				}
				current, _ := sdk.ModuleContextFrom(ctx)
				options := current.State.(config)
				if options.Trace != "" {
					file, err := os.OpenFile(options.Trace, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
					if err != nil {
						return nil, err
					}
					err = json.NewEncoder(file).Encode(map[string]any{"version": manifest.Version, "request": request, "scope": call.Scope, "component": current.Component})
					file.Close()
					if err != nil {
						return nil, err
					}
				}
				switch options.Mode {
				case "timeout":
					<-ctx.Done()
					return nil, ctx.Err()
				case "error":
					return nil, errors.New("fixture private rejection")
				case "invalid":
					return sdk.RoutingResult{Order: make([]int, len(request.Seats))}, nil
				case "state-write":
					var result sdk.StateCASResult
					if stream.HostCall(ctx, sdk.MethodStateCAS, sdk.StateCASRequest{Key: "routing.must-not-write",
						Version: "0", Value: json.RawMessage(`{"written":true}`)}, &result) == nil {
						return nil, errors.New("routing read acquired persistent write authority")
					}
				case "hold":
					if err := os.WriteFile(options.Started, []byte("ordering"), 0600); err != nil {
						return nil, err
					}
					for {
						if _, err := os.Stat(options.Release); err == nil {
							break
						}
						select {
						case <-ctx.Done():
							return nil, ctx.Err()
						case <-time.After(time.Millisecond):
						}
					}
				}
				order := make([]int, len(request.Seats))
				for index := range order {
					order[index] = len(order) - index - 1
					if manifest.Version == "2.0.0" {
						order[index] = index
					}
				}
				return sdk.RoutingResult{Order: order}, nil
			},
		},
	}
	if manifest.Component == nil {
		if action, found := manifest.ResolveAction("rank"); found && action.Component == "backend" {
			module.Actions = nil
		}
	}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil {
		os.Exit(1)
	}
}
