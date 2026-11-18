package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type settingsFile struct {
	Path       string `json:"path"`
	Body       []byte `json:"body"`
	BeforeHash string `json:"beforeHash"`
	Exists     bool   `json:"exists"`
}

func readSettings(raw json.RawMessage) (settingsFile, map[string]any, error) {
	var snapshots struct {
		Files []settingsFile `json:"files"`
	}
	if err := json.Unmarshal(raw, &snapshots); err != nil || len(snapshots.Files) != 1 {
		return settingsFile{}, nil, fmt.Errorf("fixture needs one checked setting file")
	}
	file := snapshots.Files[0]
	settings := map[string]any{}
	if file.Exists {
		if err := json.Unmarshal(file.Body, &settings); err != nil {
			return file, nil, err
		}
	}
	return file, settings, nil
}

func captureOptions(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var options map[string]json.RawMessage
	if err := json.Unmarshal(raw, &options); err != nil {
		return nil, err
	}
	captured := map[string]json.RawMessage{}
	for _, key := range []string{"models", "decisionModels"} {
		if value, present := options[key]; present {
			captured[key] = value
		}
	}
	return captured, nil
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
		sdk.MethodAgentInspect: func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			var request sdk.AgentInspectRequest
			if err := json.Unmarshal(call.Params, &request); err != nil {
				return nil, err
			}
			_, settings, err := readSettings(request.Settings)
			if err != nil {
				return nil, err
			}
			captured, err := captureOptions(request.Options)
			if err != nil {
				return nil, err
			}
			detail, err := json.Marshal(captured)
			if err != nil {
				return nil, err
			}
			connected, _ := settings["connected"].(bool)
			model, _ := settings["model"].(string)
			return sdk.AgentInspectResult{Connected: connected, Values: map[string]string{"model": model}, Detail: string(detail)}, nil
		},
		sdk.MethodAgentPlan: func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			var request sdk.AgentPlanRequest
			if err := json.Unmarshal(call.Params, &request); err != nil {
				return nil, err
			}
			file, settings, err := readSettings(request.Settings)
			if err != nil {
				return nil, err
			}
			captured, err := captureOptions(request.Options)
			if err != nil {
				return nil, err
			}
			var options struct {
				Models []struct {
					Value string `json:"value"`
				} `json:"models"`
				Value string `json:"value"`
			}
			if err := json.Unmarshal(request.Options, &options); err != nil {
				return nil, err
			}
			model, _ := settings["model"].(string)
			switch request.Action {
			case "connect":
				if len(options.Models) == 0 {
					return nil, fmt.Errorf("fixture needs a conversational model")
				}
				model = options.Models[0].Value
			case "set":
				model = options.Value
			}
			connected := request.Action != "disconnect"
			settings["model"], settings["connected"], settings["seen"], settings["intent"] = model, connected, captured, request.Action
			body, err := json.Marshal(settings)
			if err != nil {
				return nil, err
			}
			plan := sdk.AgentPlan{Files: []sdk.FileChange{{Path: file.Path, Body: body, BeforeHash: file.BeforeHash}}}
			if request.Binding != nil {
				plan.Binding = &sdk.AgentBindingChange{Revision: request.Binding.Revision, Connected: connected, Values: map[string]string{"model": model}}
			}
			return plan, nil
		},
	}}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
