package extension

import (
	"encoding/json"
	"slices"
	"testing"
)

func processManifest() Manifest {
	return Manifest{Schema: 1, ID: "process.fixture", Version: "1.0.0", Runtime: "native", Entry: "module.so",
		Contract: Contract{Version: APIVersion{Major: 1, Minor: 10},
			Capabilities: []Capability{AgentProcess, AgentWiring, ModuleRPC}},
		Agents:    []AgentDescriptor{{ID: "worker-agent", Name: "Worker agent", Bin: "worker-agent", Launch: &AgentLaunch{}}},
		Actions:   []ActionDescriptor{{ID: "run", Effect: "write", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Processes: []AgentProcessRecipe{{ID: "run", Agent: "worker-agent", Argv: []string{"app-server", "--stdio"}}},
	}
}

func TestProcessManifestRequiresOneOwnedForegroundAction(t *testing.T) {
	manifest := processManifest()
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	cloned := manifest.Clone()
	cloned.Processes[0].Argv[0] = "changed"
	public := manifest.Public()
	public.Processes[0].Argv[0] = "changed-again"
	if manifest.Processes[0].Argv[0] != "app-server" {
		t.Fatal("process recipe leaked mutable package authority")
	}
	for name, change := range map[string]func(*Manifest){
		"old-api":   func(value *Manifest) { value.Contract.Version.Minor = 9 },
		"no-recipe": func(value *Manifest) { value.Processes = nil },
		"no-capability": func(value *Manifest) {
			value.Contract.Capabilities = slices.DeleteFunc(value.Contract.Capabilities, func(capability Capability) bool { return capability == AgentProcess })
		},
		"read-action":    func(value *Manifest) { value.Actions[0].Effect = "read" },
		"unknown-action": func(value *Manifest) { value.Processes[0].ID = "other" },
		"foreign-agent":  func(value *Manifest) { value.Processes[0].Agent = "foreign" },
		"no-launch":      func(value *Manifest) { value.Agents[0].Launch = nil },
		"duplicate": func(value *Manifest) {
			value.Processes = append(value.Processes, value.Processes[0])
		},
		"no-arguments": func(value *Manifest) { value.Processes[0].Argv = nil },
		"null-byte":    func(value *Manifest) { value.Processes[0].Argv = []string{"--arg\x00more"} },
	} {
		t.Run(name, func(t *testing.T) {
			value := manifest.Clone()
			change(&value)
			if value.Validate() == nil {
				t.Fatal("invalid foreground process authority was admitted")
			}
		})
	}
}

func TestProcessManifestRejectsBackgroundActionRecipes(t *testing.T) {
	for _, provider := range []ProviderDescriptor{
		{ID: "vendor", Checkin: &AccountCheckinDescriptor{Action: "run"}},
		{ID: "vendor", AccountTasks: []AccountTaskDescriptor{{ID: "maintenance", Action: "run"}}},
	} {
		manifest := processManifest()
		manifest.Providers = []ProviderDescriptor{provider}
		if err := ValidateAgentProcesses(manifest); err == nil {
			t.Fatal("background account maintenance received foreground process authority")
		}
		manifest.Processes = nil
		manifest.Contract.Capabilities = slices.DeleteFunc(manifest.Contract.Capabilities, func(capability Capability) bool {
			return capability == AgentProcess
		})
		if err := ValidateAgentProcesses(manifest); err != nil {
			t.Fatal("unrelated account maintenance was rejected without process authority", err)
		}
	}
}
