package extension

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestAgentModelMetadataSDK19(t *testing.T) {
	manifest := Manifest{Contract: Contract{Version: APIVersion{Major: 1, Minor: 9}, Capabilities: []Capability{AgentWiring}}}
	for name, value := range map[string]AgentModelMetadata{
		"unknown": {},
		"empty":   {Efforts: []string{}},
		"known":   {MaxOutput: 393216, Efforts: []string{"low", "medium", "high"}, DefaultEffort: "medium"},
		"ultra":   {Efforts: []string{"high", "ultra"}, DefaultEffort: "ultra"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateAgentModelMetadata(manifest, value); err != nil {
				t.Fatal(err)
			}
		})
	}
	for name, value := range map[string]AgentModelMetadata{
		"negative":  {MaxOutput: -1},
		"unsafe":    {MaxOutput: 9007199254740992},
		"duplicate": {Efforts: []string{"high", "high"}},
		"foreign":   {Efforts: []string{"automatic"}},
		"removed":   {Efforts: []string{"low", "high"}, DefaultEffort: "medium"},
		"empty":     {Efforts: []string{}, DefaultEffort: "high"},
		"unknown":   {DefaultEffort: "high"},
	} {
		t.Run("refuse-"+name, func(t *testing.T) {
			if err := ValidateAgentModelMetadata(manifest, value); err == nil {
				t.Fatal("invalid effective metadata was accepted")
			}
		})
	}
	old := manifest.Clone()
	old.Contract.Version.Minor = 8
	if err := ValidateAgentModelMetadata(old, AgentModelMetadata{}); err == nil {
		t.Fatal("old SDK borrowed effective model metadata")
	}
	manifest.Contract.Capabilities = []Capability{ModelDiscovery}
	if err := ValidateAgentModelMetadata(manifest, AgentModelMetadata{}); err == nil {
		t.Fatal("model discovery borrowed agent-wiring authority")
	}
}

func TestAgentModelMetadataWireAndClone(t *testing.T) {
	for _, efforts := range [][]string{nil, {}, {"low", "high"}} {
		value := AgentModelMetadata{Efforts: efforts}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded AgentModelMetadata
		if err := json.Unmarshal(raw, &decoded); err != nil || (decoded.Efforts == nil) != (efforts == nil) ||
			!slices.Equal(decoded.Efforts, efforts) {
			t.Fatalf("effort knowledge changed on the wire: %s (%v)", raw, err)
		}
		clone := value.Clone()
		if (clone.Efforts == nil) != (value.Efforts == nil) {
			t.Fatal("metadata clone erased explicit empty levels")
		}
		if len(clone.Efforts) > 0 {
			clone.Efforts[0] = "changed"
			if value.Efforts[0] == "changed" {
				t.Fatal("metadata clone shared mutable effort levels")
			}
		}
		model := Model{ID: "fixture", Efforts: efforts}
		raw, err = json.Marshal(model)
		if err != nil {
			t.Fatal(err)
		}
		var discovered Model
		if err := json.Unmarshal(raw, &discovered); err != nil || (discovered.Efforts == nil) != (efforts == nil) {
			t.Fatalf("SDK discovery erased explicit effort knowledge: %s (%v)", raw, err)
		}
	}
}
