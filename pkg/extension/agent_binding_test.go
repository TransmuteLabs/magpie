package extension

import (
	"encoding/json"
	"testing"
)

func TestAgentBindingStateRequiresHostRevisionTargetAndDeclaredValues(t *testing.T) {
	manifest := agentMetadataFixture()
	descriptor := manifest.Agents[0]
	before := &AgentBindingSnapshot{
		Target: descriptor.Profile.Target, Revision: "18446744073709551615", Connected: true,
		Values: map[string]string{"model": "magpie/model"},
	}
	if err := ValidateAgentBindingSnapshot(manifest, descriptor, before); err != nil {
		t.Fatal(err)
	}
	change := &AgentBindingChange{Revision: before.Revision, Connected: true, Values: map[string]string{"model": "magpie/next"}}
	if err := ValidateAgentBindingChange(manifest, descriptor, before, "set", change); err != nil {
		t.Fatal(err)
	}
	for _, alter := range []func(*AgentBindingSnapshot){
		func(value *AgentBindingSnapshot) { value.Target = "another" },
		func(value *AgentBindingSnapshot) { value.Revision = "01" },
		func(value *AgentBindingSnapshot) { value.Revision = "18446744073709551616" },
		func(value *AgentBindingSnapshot) { value.Values["credentials"] = "not a field" },
		func(value *AgentBindingSnapshot) { value.Values = nil },
	} {
		value := before.Clone()
		alter(value)
		if err := ValidateAgentBindingSnapshot(manifest, descriptor, value); err == nil {
			t.Fatal("invalid host binding snapshot was accepted")
		}
	}
	stale := change.Clone()
	stale.Revision = "0"
	if err := ValidateAgentBindingChange(manifest, descriptor, before, "set", stale); err == nil {
		t.Fatal("binding change did not echo its host revision")
	}
}

func TestAgentBindingStateStrictWireShapes(t *testing.T) {
	for _, raw := range []string{
		`{"target":"fixture-base","revision":"0","values":{}}`,
		`{"target":"fixture-base","revision":"0","connected":null,"values":{}}`,
		`{"target":"fixture-base","revision":"0","connected":false,"values":{},"plugin":"another"}`,
	} {
		var snapshot AgentBindingSnapshot
		if json.Unmarshal([]byte(raw), &snapshot) == nil {
			t.Fatal("host binding accepted missing state or an authority selector", raw)
		}
	}
	manifest := agentMetadataFixture()
	descriptor := manifest.Agents[0]
	before := &AgentBindingSnapshot{Target: descriptor.Profile.Target, Revision: "0", Values: map[string]string{}}
	change := &AgentBindingChange{Revision: "0", Connected: true, Values: nil}
	if err := ValidateAgentBindingChange(manifest, descriptor, before, "connect", change); err == nil {
		t.Fatal("binding change accepted null values")
	}
	change.Values = map[string]string{}
	if err := ValidateAgentBindingChange(manifest, descriptor, before, "connect", change); err == nil {
		t.Fatal("connected model-flag launch accepted an empty model")
	}
}
