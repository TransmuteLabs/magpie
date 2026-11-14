package extension

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestAgentDecisionModelsSDK19(t *testing.T) {
	manifest := Manifest{Contract: Contract{Version: APIVersion{Major: 1, Minor: 9}, Capabilities: []Capability{AgentWiring}}}
	for _, models := range [][]AgentDecisionModel{
		{},
		{{ID: "mixed/judge", Name: "Judge", Provider: "mixed", Endpoints: []string{"/v1/systemone", "/v1/decisions"}}},
		{{ID: "group/judges", Name: "Judges", Group: true, Endpoints: []string{"/v1/systemone"}}},
	} {
		if err := ValidateAgentDecisionModels(manifest, models); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(models)
		if err != nil {
			t.Fatal(err)
		}
		var decoded []AgentDecisionModel
		if err := json.Unmarshal(raw, &decoded); err != nil || ValidateAgentDecisionModels(manifest, decoded) != nil {
			t.Fatalf("decision catalog did not round-trip: %s (%v)", raw, err)
		}
	}
	for _, contract := range []Contract{
		{Version: APIVersion{Major: 1, Minor: 8}, Capabilities: []Capability{AgentWiring}},
		{Version: APIVersion{Major: 2, Minor: 9}, Capabilities: []Capability{AgentWiring}},
		{Version: APIVersion{Major: 1, Minor: 9}, Capabilities: []Capability{ModelDiscovery}},
	} {
		if err := ValidateAgentDecisionModels(Manifest{Contract: contract}, []AgentDecisionModel{}); err == nil {
			t.Fatal("decision options borrowed an older or unrelated contract")
		}
	}
}

func TestAgentDecisionModelsRefuseInvalidFacts(t *testing.T) {
	manifest := Manifest{Contract: Contract{Version: APIVersion{Major: 1, Minor: 9}, Capabilities: []Capability{AgentWiring}}}
	if err := ValidateAgentDecisionModels(manifest, nil); err == nil {
		t.Fatal("unknown decision catalog was serialized as a known empty list")
	}
	for _, model := range []AgentDecisionModel{
		{ID: "", Name: "Judge", Provider: "mixed", Endpoints: []string{"/v1/systemone"}},
		{ID: "mixed/judge", Name: " ", Provider: "mixed", Endpoints: []string{"/v1/systemone"}},
		{ID: "mixed/judge", Name: "Judge", Endpoints: []string{"/v1/systemone"}},
		{ID: "group/judges", Name: "Judges", Group: true, Provider: "mixed", Endpoints: []string{"/v1/systemone"}},
		{ID: "mixed/judge", Name: "Judge", Provider: "mixed"},
		{ID: "mixed/judge", Name: "Judge", Provider: "mixed", Endpoints: []string{"/v1/chat/completions"}},
		{ID: "mixed/judge", Name: "Judge", Provider: "mixed", Endpoints: []string{"https://vendor.invalid/v1/systemone"}},
		{ID: "mixed/judge", Name: "Judge", Provider: "mixed", Endpoints: []string{"/v1/systemone?key=private"}},
		{ID: "mixed/judge", Name: "Judge", Provider: "mixed", Endpoints: []string{"/v1/systemone", "/v1/systemone"}},
	} {
		if err := ValidateAgentDecisionModels(manifest, []AgentDecisionModel{model}); err == nil {
			t.Fatalf("invalid decision fact was accepted: %+v", model)
		}
	}
}

func TestAgentDecisionModelsWireAndClone(t *testing.T) {
	model := AgentDecisionModel{ID: "mixed/judge", Name: "Judge", Provider: "mixed",
		Endpoints: []string{"/v1/systemone", "/v1/decisions"}}
	clone := model.Clone()
	clone.Endpoints[0] = "changed"
	if model.Endpoints[0] != "/v1/systemone" {
		t.Fatal("decision model clone shared endpoint authority")
	}
	raw, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for field := range fields {
		if !slices.Contains([]string{"id", "name", "provider", "endpoints"}, field) {
			t.Fatalf("decision DTO exposed an unexpected field: %s", field)
		}
	}
}
