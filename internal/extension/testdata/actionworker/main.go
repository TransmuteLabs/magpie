// A schema owned by the fixture module, never interpreted by the host.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type snapshot struct {
	Path       string `json:"path"`
	Body       []byte `json:"body"`
	BeforeHash string `json:"beforeHash"`
	Exists     bool   `json:"exists"`
}

type options struct {
	Gateway  string     `json:"gateway"`
	Key      string     `json:"key"`
	Field    string     `json:"field"`
	Value    string     `json:"value"`
	Previous []snapshot `json:"previous"`
}

func settings(raw json.RawMessage) (snapshot, map[string]string, error) {
	var files struct {
		Files []snapshot `json:"files"`
	}
	if err := json.Unmarshal(raw, &files); err != nil || len(files.Files) == 0 {
		return snapshot{}, nil, fmt.Errorf("missing host snapshots")
	}
	file := files.Files[0]
	values := map[string]string{}
	if file.Exists {
		if err := json.Unmarshal(file.Body, &values); err != nil {
			return file, nil, err
		}
	}
	return file, values, nil
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
	module := sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{}}
	if manifest.Contract.Has(sdk.AgentWiring) {
		module.Handlers[sdk.MethodAgentInspect] = func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			var request sdk.AgentInspectRequest
			if err := json.Unmarshal(call.Params, &request); err != nil {
				return nil, err
			}
			_, values, err := settings(request.Settings)
			if err != nil {
				return nil, err
			}
			return sdk.AgentInspectResult{Connected: values["gateway"] != "", Values: map[string]string{"model": values["model"]}}, nil
		}
		module.Handlers[sdk.MethodAgentPlan] = func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			var request sdk.AgentPlanRequest
			if err := json.Unmarshal(call.Params, &request); err != nil {
				return nil, err
			}
			file, values, err := settings(request.Settings)
			if err != nil {
				return nil, err
			}
			var opts options
			if err := json.Unmarshal(request.Options, &opts); err != nil {
				return nil, err
			}
			switch request.Action {
			case "connect", "sync", "set":
				values["gateway"], values["key"] = opts.Gateway, opts.Key
				if request.Action == "set" {
					values[opts.Field] = opts.Value
				}
			case "disconnect":
				previous := map[string]string{}
				if len(opts.Previous) != 0 && opts.Previous[0].Exists {
					if err := json.Unmarshal(opts.Previous[0].Body, &previous); err != nil {
						return nil, err
					}
				}
				for _, key := range []string{"gateway", "key", "model"} {
					if key == "model" && !strings.HasPrefix(values[key], "magpie/") {
						continue
					}
					if value, exists := previous[key]; exists {
						values[key] = value
					} else {
						delete(values, key)
					}
				}
			default:
				return nil, fmt.Errorf("unexpected action")
			}
			body, err := json.Marshal(values)
			if err != nil {
				return nil, err
			}
			return sdk.AgentPlan{Files: []sdk.FileChange{{Path: file.Path, Body: body, BeforeHash: file.BeforeHash}}}, nil
		}
	}
	if manifest.Contract.Has(sdk.SessionMigration) {
		module.Handlers[sdk.MethodSessionConvert] = func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			var request sdk.SessionRequest
			if err := json.Unmarshal(call.Params, &request); err != nil {
				return nil, err
			}
			body, err := json.Marshal(struct {
				Source  []sdk.SessionDocument `json:"source"`
				Options json.RawMessage       `json:"options"`
			}{request.Files, request.Options})
			if err != nil {
				return nil, err
			}
			return sdk.SessionResult{ID: "converted-session", Files: []sdk.SessionDocument{
				{Path: "converted-session.jsonl", Format: request.To, Body: body},
			}}, nil
		}
	}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, module); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
