package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func agentDecisionModelsFixture(t *testing.T, controlled bool, minor int, decisions bool) (*Agent, string) {
	t.Helper()
	descriptor := sdk.AgentDescriptor{ID: "fixture-decisions", Name: "Decision catalog fixture", Bin: "fixture-decision-editor",
		ConfigFiles: []string{".decision-fixture/settings.json"}, Fields: []sdk.AgentField{{Key: "model", Label: "Model"}}}
	if controlled {
		descriptor.Launch = &sdk.AgentLaunch{}
	}
	installAgentWorkerFixture(t, descriptor, "./testdata/decisionmodelsworker", minor)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".decision-fixture", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"note":"keep"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mixed := provider.Provider{ID: "mixed", Name: "Mixed", Key: "fixture-private-key",
		Chat: "http://127.0.0.1:1/v1", Models: []string{"conversation"}}
	if decisions {
		mixed.Decide = mixed.Chat
		mixed.Models = append(mixed.Models, "typesafe/jev")
	}
	if err := provider.Save(mixed); err != nil {
		t.Fatal(err)
	}
	if decisions {
		for _, group := range []provider.Group{
			{ID: "judges", Name: "Judges", Members: []string{"mixed/typesafe/jev"}},
			{ID: "disabled-judges", Name: "Disabled judges", Members: []string{"mixed/typesafe/jev"}},
		} {
			if err := provider.SaveGroup(group); err != nil {
				t.Fatal(err)
			}
		}
		if err := provider.SwitchGroup("disabled-judges", false); err != nil {
			t.Fatal(err)
		}
	}
	agent, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	return agent, path
}

func requireAgentDecisionModels(t *testing.T, raw []byte, agentID string, minor int) {
	t.Helper()
	var seen map[string]json.RawMessage
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatalf("agent options were not observed: %s: %v", raw, err)
	}
	decisions, present := seen["decisionModels"]
	if minor < 9 {
		if present {
			t.Errorf("SDK 1.%d received API9 decisionModels: %s", minor, decisions)
		}
		return
	}
	if !present || string(decisions) == "null" {
		t.Errorf("ordinary SDK agent lost API9 decisionModels: %s", raw)
		return
	}
	var got, want any
	if err := json.Unmarshal(decisions, &got); err != nil {
		t.Fatal(err)
	}
	expected, err := json.Marshal(pluggedDecisionModels(agentID))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(expected, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SDK decision catalog differs from the upstream owner: got %s, want %s", decisions, expected)
	}
	var models []struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(seen["models"], &models); err != nil {
		t.Fatal(err)
	}
	foundChat := false
	for _, model := range models {
		if model.Value == "magpie/mixed/typesafe/jev" || model.Value == "magpie/group/judges" {
			t.Errorf("decision model became a conversational option: %s", model.Value)
		}
		foundChat = foundChat || model.Value == "magpie/mixed/conversation"
	}
	if !foundChat {
		t.Error("the decision projection lost the provider's conversational model")
	}
}

func readDecisionModelsPlan(t *testing.T, path, agentID string, minor int, intent string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Note   string          `json:"note"`
		Intent string          `json:"intent"`
		Seen   json.RawMessage `json:"seen"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil || settings.Note != "keep" || settings.Intent != intent {
		t.Fatalf("ordinary %s did not preserve the user's config: %s (%v)", intent, raw, err)
	}
	requireAgentDecisionModels(t, settings.Seen, agentID, minor)
}

func TestExtensionAgentDecisionModelsSDK19(t *testing.T) {
	for _, controlled := range []bool{false, true} {
		name := "ordinary"
		if controlled {
			name = "controlled"
		}
		t.Run(name, func(t *testing.T) {
			agent, path := agentDecisionModelsFixture(t, controlled, 9, true)
			if len(pluggedDecisionModels(agent.ID)) != 2 {
				raw, _ := json.Marshal(pluggedDecisionModels(agent.ID))
				t.Fatalf("fixture did not expose its decision model and visible group: %s", raw)
			}
			if _, err := agent.ConnectHow(); err != nil {
				t.Fatal(err)
			}
			readDecisionModelsPlan(t, path, agent.ID, 9, "connect")
			state := agent.Native.Read()
			if state.Provider != "connected" {
				t.Fatalf("ordinary inspection lost the connection: %+v", state)
			}
			requireAgentDecisionModels(t, []byte(state.Detail), agent.ID, 9)
			if err := agent.Apply("model", "magpie/mixed/conversation"); err != nil {
				t.Fatal(err)
			}
			readDecisionModelsPlan(t, path, agent.ID, 9, "set")
			if err := provider.SwitchGroup("judges", false); err != nil {
				t.Fatal(err)
			}
			if err := agent.Sync(); err != nil {
				t.Fatal(err)
			}
			readDecisionModelsPlan(t, path, agent.ID, 9, "sync")
			requireAgentDecisionModels(t, []byte(agent.Native.Read().Detail), agent.ID, 9)
		})
	}
}

func TestExtensionAgentDecisionModelsLegacyOff(t *testing.T) {
	for _, controlled := range []bool{false, true} {
		name := "ordinary"
		if controlled {
			name = "controlled"
		}
		t.Run(name, func(t *testing.T) {
			agent, path := agentDecisionModelsFixture(t, controlled, 8, true)
			if _, err := agent.ConnectHow(); err != nil {
				t.Fatal(err)
			}
			readDecisionModelsPlan(t, path, agent.ID, 8, "connect")
			requireAgentDecisionModels(t, []byte(agent.Native.Read().Detail), agent.ID, 8)
		})
	}
}

func TestExtensionAgentDecisionModelsEmptySDK19(t *testing.T) {
	for _, controlled := range []bool{false, true} {
		name := "ordinary"
		if controlled {
			name = "controlled"
		}
		t.Run(name, func(t *testing.T) {
			agent, path := agentDecisionModelsFixture(t, controlled, 9, false)
			if len(pluggedDecisionModels(agent.ID)) != 0 {
				t.Fatal("empty fixture has an unexpected decision provider")
			}
			if _, err := agent.ConnectHow(); err != nil {
				t.Fatal(err)
			}
			readDecisionModelsPlan(t, path, agent.ID, 9, "connect")
			requireAgentDecisionModels(t, []byte(agent.Native.Read().Detail), agent.ID, 9)
		})
	}
}

func TestAgentDecisionModelsCallerCannotMutateOwner(t *testing.T) {
	agent, _ := agentDecisionModelsFixture(t, false, 9, true)
	manifest := sdk.Manifest{Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 9},
		Capabilities: []sdk.Capability{sdk.AgentWiring}}}
	models, err := extensionAgentDecisionModels(agent.ID, manifest)
	if err != nil || len(models) == 0 {
		t.Fatalf("decision projection missing: %+v %v", models, err)
	}
	models[0].Endpoints[0] = "changed"
	fresh, err := extensionAgentDecisionModels(agent.ID, manifest)
	if err != nil || fresh[0].Endpoints[0] != "/v1/systemone" {
		t.Fatal("a caller changed the upstream decision endpoint authority", err)
	}
}
