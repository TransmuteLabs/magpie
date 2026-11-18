package extension

import (
	"slices"
	"testing"
)

func TestActionDescriptorsOrdinaryMetadata(t *testing.T) {
	manifest := testManifest(AgentWiring, SessionMigration)
	manifest.Agents = []AgentDescriptor{{ID: "editor", Name: "Editor", Bin: "editor",
		ConfigFiles: []string{".editor/config.json"}, Fields: []AgentField{{Key: "model", Label: "model"}}}}
	manifest.Sessions = []SessionDescriptor{{ID: "convert", Name: "Convert", From: "source", To: "target", OutputDir: ".target/sessions"}}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(RequiredMethods(manifest), MethodAgentInspect) {
		t.Fatal("ordinary wiring did not negotiate inspection")
	}
	if capability, ok := MethodCapability(MethodAgentInspect); !ok || capability != AgentWiring {
		t.Fatal("inspection lacks agent authority")
	}
	copy := manifest.Public()
	copy.Agents[0].ConfigFiles[0] = ".other"
	copy.Agents[0].Fields[0].Key = "other"
	if manifest.Agents[0].ConfigFiles[0] != ".editor/config.json" || manifest.Agents[0].Fields[0].Key != "model" {
		t.Fatal("public manifest aliases wiring metadata")
	}
	manual := testManifest(AgentWiring)
	manual.Agents = []AgentDescriptor{{ID: "manual", Name: "Manual"}}
	if err := manual.Validate(); err != nil || slices.Contains(RequiredMethods(manual), MethodAgentInspect) {
		t.Fatalf("manual-only compatibility: %v", err)
	}
	for _, bad := range []string{"/tmp/config", "../config", ".x/../config", `C:\config`, ".x/*.json", ".x/.magpie-extension.lock", ".x\nbad"} {
		t.Run(bad, func(t *testing.T) {
			copy := manifest.Public()
			copy.Agents[0].ConfigFiles = []string{bad}
			if copy.Validate() == nil {
				t.Fatalf("unsafe agent path accepted: %q", bad)
			}
			copy = manifest.Public()
			copy.Sessions[0].OutputDir = bad
			if copy.Validate() == nil {
				t.Fatalf("unsafe session output accepted: %q", bad)
			}
		})
	}
	for _, change := range []func(*Manifest){
		func(m *Manifest) { m.Agents[0].ConfigFiles = append(m.Agents[0].ConfigFiles, ".EDITOR/config.json") },
		func(m *Manifest) { m.Agents[0].Fields = append(m.Agents[0].Fields, m.Agents[0].Fields[0]) },
		func(m *Manifest) { m.Agents[0].Bin = "../editor" },
		func(m *Manifest) { m.Sessions[0].To = "" },
	} {
		copy := manifest.Public()
		change(&copy)
		if copy.Validate() == nil {
			t.Fatal("invalid ordinary metadata accepted")
		}
	}
}
