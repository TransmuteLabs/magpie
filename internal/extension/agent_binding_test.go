package extension

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func bindingActionFixture(test *testing.T) (*Actions, *actionTestRuntime, ActionPolicy) {
	test.Helper()
	actions, host, policy := actionFixture(test)
	host.manifest.Agents[0].Bin = "fixture-editor"
	host.manifest.Agents[0].Launch = &sdk.AgentLaunch{Env: map[string]string{"FIXTURE_URL": "{url}"}}
	policy.Paths = nil
	policy.Binding = &sdk.AgentBindingSnapshot{Target: "editor", Revision: "0", Values: map[string]string{}}
	host.invoke = func(_ context.Context, method string, _ sdk.Scope, params any) (any, error) {
		switch method {
		case sdk.MethodAgentInspect:
			request := params.(sdk.AgentInspectRequest)
			if request.Binding == nil || request.Binding.Target != "editor" || request.Binding.Revision != "0" {
				test.Fatal("inspection trusted a caller binding instead of its host snapshot")
			}
			return sdk.AgentInspectResult{Values: request.Binding.Values}, nil
		case sdk.MethodAgentPlan:
			request := params.(sdk.AgentPlanRequest)
			if request.Binding == nil || request.Binding.Target != "editor" || request.Binding.Revision != "0" {
				test.Fatal("planning trusted a caller binding instead of its host snapshot")
			}
			return sdk.AgentPlan{Binding: &sdk.AgentBindingChange{
				Revision: request.Binding.Revision, Connected: true, Values: map[string]string{},
			}}, nil
		default:
			test.Fatal("unexpected binding action", method)
			return nil, nil
		}
	}
	return actions, host, policy
}

func TestAgentBindingActionsUseHostSnapshotsWithoutInventedFiles(test *testing.T) {
	actions, _, policy := bindingActionFixture(test)
	forged := &sdk.AgentBindingSnapshot{Target: "another", Revision: "99", Connected: true}
	if _, err := actions.AgentInspect(test.Context(), "sample", sdk.Scope{},
		sdk.AgentInspectRequest{ID: "editor", Binding: forged}, policy); err != nil {
		test.Fatal(err)
	}
	plan, err := actions.AgentPlan(test.Context(), "sample", sdk.Scope{},
		sdk.AgentPlanRequest{ID: "editor", Action: "connect", Binding: forged}, policy)
	if err != nil {
		test.Fatal(err)
	}
	preview := plan.Preview()
	if len(preview.Files) != 0 || preview.Binding == nil || !preview.Binding.Connected || len(plan.Snapshots()) != 0 {
		test.Fatal("binding-only action manufactured an editable config file")
	}
	preview.Binding.Values["forged"] = "value"
	if plan.Preview().Binding.Values["forged"] != "" {
		test.Fatal("preview exposed mutable binding authority")
	}
	result, err := plan.Execute(test.Context(), plan.Preview().Revision)
	if err != nil || !result.Applied || result.Backup != "" {
		test.Fatalf("empty-file binding approval: %+v %v", result, err)
	}
	for _, name := range []string{"a.json", "b.json"} {
		raw, err := os.ReadFile(filepath.Join(policy.Root, name))
		if err != nil || string(raw) != "before-"+name {
			test.Fatal("binding-only execution touched unrelated files")
		}
	}
}

func TestAgentBindingActionsCannotExpandEmptyFileAuthority(test *testing.T) {
	for _, scenario := range []string{"missing snapshot", "ordinary descriptor", "forged file", "stale revision", "undeclared value"} {
		test.Run(scenario, func(test *testing.T) {
			actions, host, policy := bindingActionFixture(test)
			switch scenario {
			case "missing snapshot":
				policy.Binding = nil
			case "ordinary descriptor":
				host.manifest.Agents[0].Launch = nil
			default:
				host.invoke = func(context.Context, string, sdk.Scope, any) (any, error) {
					result := sdk.AgentPlan{Binding: &sdk.AgentBindingChange{Revision: "0", Connected: true, Values: map[string]string{}}}
					switch scenario {
					case "forged file":
						result.Files = []sdk.FileChange{{Path: "a.json", Body: []byte("escaped")}}
					case "stale revision":
						result.Binding.Revision = "1"
					case "undeclared value":
						result.Binding.Values = map[string]string{"credentials": "not a field"}
					}
					return result, nil
				}
			}
			if _, err := actions.AgentPlan(test.Context(), "sample", sdk.Scope{},
				sdk.AgentPlanRequest{ID: "editor", Action: "connect"}, policy); err == nil {
				test.Fatal("binding action widened its declared authority")
			}
		})
	}
}

func TestAgentBindingConfirmationIncludesHostState(test *testing.T) {
	actions, host, policy := bindingActionFixture(test)
	host.invoke = func(_ context.Context, _ string, _ sdk.Scope, params any) (any, error) {
		request := params.(sdk.AgentPlanRequest)
		return sdk.AgentPlan{Binding: &sdk.AgentBindingChange{Revision: request.Binding.Revision, Connected: true, Values: map[string]string{}}}, nil
	}
	first, err := actions.AgentPlan(test.Context(), "sample", sdk.Scope{},
		sdk.AgentPlanRequest{ID: "editor", Action: "connect"}, policy)
	if err != nil {
		test.Fatal(err)
	}
	policy.Binding.Revision = "1"
	second, err := actions.AgentPlan(test.Context(), "sample", sdk.Scope{},
		sdk.AgentPlanRequest{ID: "editor", Action: "connect"}, policy)
	if err != nil {
		test.Fatal(err)
	}
	if first.Preview().Revision == second.Preview().Revision {
		test.Fatal("binding confirmation could be reused after host state changed")
	}
}

func TestAgentBindingChangeRejectsMissingConnectedAndSelectors(test *testing.T) {
	for _, raw := range []string{
		`{"revision":"0","values":{}}`,
		`{"revision":"0","connected":null,"values":{}}`,
		`{"revision":"0","connected":true,"target":"another","values":{}}`,
	} {
		var value sdk.AgentBindingChange
		if json.Unmarshal([]byte(raw), &value) == nil {
			test.Fatal("binding result accepted missing state or an authority selector")
		}
	}
}
