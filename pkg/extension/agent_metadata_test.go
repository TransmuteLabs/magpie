package extension

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func TestRichAgentMetadataRequiresInspectWithoutChangingPlainPlans(t *testing.T) {
	for _, descriptor := range []AgentDescriptor{
		{ID: "plain", Name: "Plain"},
		{ID: "binary", Name: "Binary", Bin: "editor"},
		{ID: "config", Name: "Config", ConfigFiles: []string{".editor/config.json"}},
		{ID: "fields", Name: "Fields", Fields: []AgentField{{Key: "model", Label: "Model"}}},
	} {
		t.Run(descriptor.ID, func(t *testing.T) {
			manifest := testManifest(AgentWiring)
			manifest.Agents = []AgentDescriptor{descriptor}
			if err := manifest.Validate(); err != nil {
				t.Fatal("optional metadata was refused", err)
			}
			rich := descriptor.Bin != "" || len(descriptor.ConfigFiles) > 0 || len(descriptor.Fields) > 0
			methods := RequiredMethods(manifest)
			if !slices.Contains(methods, MethodAgentPlan) || slices.Contains(methods, MethodAgentInspect) != rich {
				t.Fatalf("wrong required methods for %+v: %v", descriptor, methods)
			}
			module := Module{Manifest: manifest, Handlers: map[string]Handler{MethodAgentPlan: testNoop}}
			if rich {
				if err := module.Validate(); err == nil {
					t.Fatal("rich agent advertised inspection without a handler")
				}
				module.Handlers[MethodAgentInspect] = testNoop
			}
			if err := module.Validate(); err != nil {
				t.Fatal("valid wiring module was refused", err)
			}
		})
	}
	if capability, found := MethodCapability(MethodAgentInspect); !found || capability != AgentWiring {
		t.Fatal("agent.inspect lost its wiring capability")
	}
}

func TestRichActionMetadataValidatesActualPathsFieldsAndSessionRoutes(t *testing.T) {
	base := testManifest(AgentWiring, SessionMigration)
	base.Agents = []AgentDescriptor{{ID: "editor", Name: "Editor", Bin: "editor",
		ConfigFiles: []string{".editor/config.json"}, Fields: []AgentField{{Key: "model", Label: "Model"}}}}
	base.Sessions = []SessionDescriptor{{ID: "convert", Name: "Convert", From: "source", To: "target", OutputDir: ".target/sessions"}}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"/outside", "../outside", ".editor/../outside", `C:\outside`, `..\outside`,
		".editor/*.json", ".editor/[config]", ".editor//config", ".editor/.magpie-extension.lock", "config\nbad"} {
		t.Run(bad, func(t *testing.T) {
			manifest := cloneManifest(base)
			manifest.Agents[0].ConfigFiles = []string{bad}
			if err := manifest.Validate(); err == nil {
				t.Fatal("unsafe config path accepted", bad)
			}
			manifest = cloneManifest(base)
			manifest.Sessions[0].OutputDir = bad
			if err := manifest.Validate(); err == nil {
				t.Fatal("unsafe session path accepted", bad)
			}
		})
	}
	for _, change := range []func(*Manifest){
		func(manifest *Manifest) { manifest.Agents[0].Bin = "../editor" },
		func(manifest *Manifest) {
			manifest.Agents[0].ConfigFiles = append(manifest.Agents[0].ConfigFiles, ".EDITOR/config.json")
		},
		func(manifest *Manifest) {
			manifest.Agents[0].Fields = append(manifest.Agents[0].Fields, manifest.Agents[0].Fields[0])
		},
		func(manifest *Manifest) { manifest.Agents[0].Fields[0].Key = "invalid/key" },
		func(manifest *Manifest) { manifest.Agents[0].Fields[0].Label = "invalid\nlabel" },
		func(manifest *Manifest) { manifest.Sessions[0].From = "" },
		func(manifest *Manifest) { manifest.Sessions[0].To = "source" },
		func(manifest *Manifest) { manifest.Sessions[0].OutputDir = "" },
	} {
		manifest := cloneManifest(base)
		change(&manifest)
		if err := manifest.Validate(); err == nil {
			t.Fatalf("invalid action metadata accepted: %+v %+v", manifest.Agents, manifest.Sessions)
		}
	}
}

func TestActionMetadataMarshalAndPublicSnapshotsDoNotShareSlices(t *testing.T) {
	manifest := testManifest(AgentWiring, SessionMigration)
	manifest.Agents = []AgentDescriptor{{ID: "editor", Name: "Editor", Bin: "editor",
		ConfigFiles: []string{".editor/config.json"}, Fields: []AgentField{{Key: "model", Label: "Model"}}}}
	manifest.Sessions = []SessionDescriptor{{ID: "convert", Name: "Convert", From: "source", To: "target", OutputDir: ".target/sessions"}}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip Manifest
	if err := json.Unmarshal(body, &roundtrip); err != nil || !reflect.DeepEqual(roundtrip, manifest) {
		t.Fatalf("action descriptor wire fields changed: %+v (%v)", roundtrip, err)
	}
	public := manifest.Public()
	manifest.Agents[0].ConfigFiles[0] = ".changed"
	manifest.Agents[0].Fields[0].Label = "Changed"
	if public.Agents[0].ConfigFiles[0] != ".editor/config.json" || public.Agents[0].Fields[0].Label != "Model" {
		t.Fatal("public snapshot aliases source metadata")
	}
	public.Agents[0].Fields[0].Key = "changed"
	if manifest.Agents[0].Fields[0].Key != "model" {
		t.Fatal("source metadata aliases public snapshot")
	}
	inspect := AgentInspectResult{Connected: true, Values: map[string]string{"model": "selected"}, Detail: "Available"}
	var inspected AgentInspectResult
	body, _ = json.Marshal(inspect)
	if err := json.Unmarshal(body, &inspected); err != nil || !reflect.DeepEqual(inspected, inspect) {
		t.Fatal("agent inspection DTO changed", err)
	}
	converted := SessionResult{ID: "converted", Files: []SessionDocument{{Path: "session.json", Format: "target", Body: []byte("session")}}}
	var session SessionResult
	body, _ = json.Marshal(converted)
	if err := json.Unmarshal(body, &session); err != nil || !reflect.DeepEqual(session, converted) {
		t.Fatal("session result lost its converted id", err)
	}
}

func TestDescribeFreezesNestedAgentMetadataBeforeInspection(t *testing.T) {
	manifest := testManifest(AgentWiring)
	manifest.Agents = []AgentDescriptor{{ID: "editor", Name: "Editor", Bin: "editor",
		ConfigFiles: []string{".editor/config.json"}, Fields: []AgentField{{Key: "model", Label: "Model"}}}}
	module := Module{Manifest: manifest, Handlers: map[string]Handler{MethodAgentPlan: testNoop, MethodAgentInspect: testNoop}}
	handlers, _, err := module.prepare()
	if err != nil {
		t.Fatal(err)
	}
	manifest.Agents[0].ConfigFiles[0] = ".changed"
	manifest.Agents[0].Fields[0].Label = "Changed"
	first, err := handlers[MethodDescribe](context.Background(), Call{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := first.(Manifest)
	if snapshot.Agents[0].ConfigFiles[0] != ".editor/config.json" || snapshot.Agents[0].Fields[0].Label != "Model" {
		t.Fatal("worker handshake did not freeze nested metadata")
	}
	snapshot.Agents[0].Fields[0].Label = "Another change"
	second, _ := handlers[MethodDescribe](context.Background(), Call{}, nil)
	if second.(Manifest).Agents[0].Fields[0].Label != "Model" {
		t.Fatal("one describe call mutated the next handshake")
	}
}
