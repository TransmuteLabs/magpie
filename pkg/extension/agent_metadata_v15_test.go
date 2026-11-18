package extension

import (
	"encoding/json"
	"testing"
)

func agentMetadataFixture() Manifest {
	return Manifest{
		Schema: ManifestSchema, ID: "fixture-agent", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract: Contract{Version: APIVersion{Major: 1, Minor: 5}, Capabilities: []Capability{AgentWiring}},
		Agents: []AgentDescriptor{{
			ID: "native-profile", Name: "Native profile", Bin: "fixture-editor",
			Fields:  []AgentField{{Key: "model", Label: "Model"}},
			Aliases: []string{"native-profile-cli"}, UA: []string{"fixture-agent/"},
			Launch: &AgentLaunch{
				Env:  map[string]string{"FIXTURE_URL": "{url}", "FIXTURE_KEY": "{key}"},
				Args: map[string]string{"--mode": "native"}, Model: "--model", Plain: []string{"login"},
			},
			Profile: &AgentProfile{Target: "fixture-base"},
			Library: &AgentLibrary{
				Instructions: ".fixture/AGENTS.md", MCP: ".fixture/mcp.json", MCPFormat: "claude", Skills: ".fixture/skills",
			},
		}},
	}
}

func TestAgentMetadataRequiresFiveAndPreservesLegacy(t *testing.T) {
	manifest := agentMetadataFixture()
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	manifest.Contract.Version.Minor = 4
	if err := manifest.Validate(); err == nil {
		t.Fatal("SDK 1.4 accepted launch/profile metadata")
	}
	manifest.Agents[0] = AgentDescriptor{ID: "editor", Name: "Editor"}
	if err := manifest.Validate(); err != nil {
		t.Fatal("legacy ID/name descriptor stopped working:", err)
	}
}

func TestAgentMetadataRejectsInvalidAuthorityAndLaunch(t *testing.T) {
	for name, alter := range map[string]func(*Manifest){
		"self profile":      func(value *Manifest) { value.Agents[0].Profile.Target = value.Agents[0].ID },
		"root profile":      func(value *Manifest) { value.Agents[0].Profile.Target = "magpie" },
		"duplicate aliases": func(value *Manifest) { value.Agents[0].Aliases = []string{"same", "same"} },
		"own alias":         func(value *Manifest) { value.Agents[0].Aliases = []string{value.Agents[0].ID} },
		"foreign descriptor alias": func(value *Manifest) {
			value.Agents = append(value.Agents, AgentDescriptor{ID: "native-profile-cli", Name: "Another"})
		},
		"unsafe environment name": func(value *Manifest) { value.Agents[0].Launch.Env["BAD-NAME"] = "value" },
		"control environment":     func(value *Manifest) { value.Agents[0].Launch.Env["VALUE"] = "x\ncommand" },
		"unsafe flag":             func(value *Manifest) { value.Agents[0].Launch.Model = "--model;command" },
		"plain shell pattern":     func(value *Manifest) { value.Agents[0].Launch.Plain = []string{"*"} },
		"missing launch binary": func(value *Manifest) {
			value.Agents[0].Bin, value.Agents[0].Profile = "", nil
		},
		"configuration not declared": func(value *Manifest) {
			value.Agents[0].Profile = nil
			value.Agents[0].Launch.Args["--settings"] = "{config}"
		},
		"escaping library":     func(value *Manifest) { value.Agents[0].Library.Skills = "../outside" },
		"absolute library":     func(value *Manifest) { value.Agents[0].Library.MCP = "/etc/mcp.json" },
		"missing MCP format":   func(value *Manifest) { value.Agents[0].Library.MCPFormat = "" },
		"empty user agent":     func(value *Manifest) { value.Agents[0].UA = []string{""} },
		"duplicate user agent": func(value *Manifest) { value.Agents[0].UA = []string{"Agent/", "agent/"} },
	} {
		t.Run(name, func(t *testing.T) {
			manifest := agentMetadataFixture()
			alter(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatal("invalid agent metadata was accepted")
			}
		})
	}
}

func TestAgentMetadataCloneOwnsNestedDeclarations(t *testing.T) {
	original := agentMetadataFixture()
	clone := original.Clone()
	clone.Agents[0].Aliases[0] = "changed"
	clone.Agents[0].UA[0] = "changed/"
	clone.Agents[0].Launch.Env["FIXTURE_URL"] = "changed"
	clone.Agents[0].Launch.Args["--mode"] = "changed"
	clone.Agents[0].Launch.Plain[0] = "changed"
	clone.Agents[0].Profile.Target = "changed"
	clone.Agents[0].Library.Skills = ".changed"
	before, _ := json.Marshal(original)
	expected, _ := json.Marshal(agentMetadataFixture())
	if string(before) != string(expected) {
		t.Fatal("cloned agent metadata mutated its installed authority")
	}
}
