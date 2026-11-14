package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type options struct {
	Models []json.RawMessage `json:"models"`
	Field  string            `json:"field"`
	Value  string            `json:"value"`
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
	module := sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{
		sdk.MethodAgentPlan: func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			var request sdk.AgentPlanRequest
			if err := json.Unmarshal(call.Params, &request); err != nil {
				return nil, err
			}
			var opts options
			if err := json.Unmarshal(request.Options, &opts); err != nil {
				return nil, err
			}
			var snapshots struct {
				Files []struct {
					Path       string `json:"path"`
					Body       []byte `json:"body"`
					BeforeHash string `json:"beforeHash"`
					Exists     bool   `json:"exists"`
				} `json:"files"`
			}
			if err := json.Unmarshal(request.Settings, &snapshots); err != nil || len(snapshots.Files) != 1 || request.Binding == nil {
				return nil, fmt.Errorf("fixture needs checked settings and binding")
			}
			file := snapshots.Files[0]
			settings := map[string]any{}
			if file.Exists {
				if err := json.Unmarshal(file.Body, &settings); err != nil {
					return nil, err
				}
			}
			binding := &sdk.AgentBindingChange{Revision: request.Binding.Revision, Values: request.Binding.Clone().Values, Connected: true}
			settings["models"], settings["intent"] = opts.Models, request.Action
			switch request.Action {
			case "connect":
				if len(opts.Models) == 0 {
					return nil, fmt.Errorf("fixture has no ordinary models")
				}
				var model struct {
					Value string `json:"value"`
				}
				if err := json.Unmarshal(opts.Models[0], &model); err != nil {
					return nil, err
				}
				binding.Values["model"] = model.Value
			case "set":
				binding.Values[opts.Field] = opts.Value
			case "disconnect":
				binding.Connected = false
			}
			settings["selected"] = binding.Values
			body, err := json.Marshal(settings)
			if err != nil {
				return nil, err
			}
			return sdk.AgentPlan{Binding: binding, Files: []sdk.FileChange{{Path: file.Path, Body: body, BeforeHash: file.BeforeHash}}}, nil
		},
		sdk.MethodAgentInspect: func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			var request sdk.AgentInspectRequest
			if err := json.Unmarshal(call.Params, &request); err != nil {
				return nil, err
			}
			var opts options
			if err := json.Unmarshal(request.Options, &opts); err != nil || len(opts.Models) == 0 || request.Binding == nil {
				return nil, fmt.Errorf("ordinary inspection lost model options")
			}
			return sdk.AgentInspectResult{Connected: request.Binding.Connected, Values: request.Binding.Clone().Values}, nil
		},
	}}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
