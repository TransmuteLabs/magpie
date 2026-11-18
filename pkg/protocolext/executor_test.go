package protocolext

import (
	"encoding/json"
	"testing"
)

func TestExecutorPolicyIsExplicitAndConversationOnly(t *testing.T) {
	for _, protocol := range []Protocol{Chat, Responses, Messages, Anthropic, Gemini} {
		if err := ValidateExecutor(SameModuleExecutor, protocol); err != nil {
			t.Errorf("%s refused its executor constraint: %v", protocol, err)
		}
	}
	for _, protocol := range []Protocol{Models, Tool, ""} {
		if err := ValidateExecutor(SameModuleExecutor, protocol); err == nil {
			t.Errorf("%s accepted a conversation executor constraint", protocol)
		}
	}
	if err := ValidateExecutor("another-module", Chat); err == nil {
		t.Fatal("accepted an arbitrary executor selector")
	}
	if err := ValidateExecutor("", Models); err != nil {
		t.Fatal("changed an unrestricted request", err)
	}
	contract := ExecutorContract()
	if err := HostContract().Check(contract); err != nil || !contract.Has(ExecutorBinding) {
		t.Fatalf("executor contract is not negotiated: %+v %v", contract, err)
	}
	contract.Version.Minor = 3
	if err := HostContract().Check(contract); err == nil {
		t.Fatal("a legacy codec acquired executor binding")
	}
}

func TestExecutorPolicyRejectsMalformedWireValues(t *testing.T) {
	for _, raw := range []string{`null`, `1`, `{}`, `[]`, `"another-module"`} {
		var policy ExecutorPolicy
		if err := json.Unmarshal([]byte(raw), &policy); err == nil {
			t.Errorf("accepted malformed executor policy %s", raw)
		}
	}
	for _, raw := range []string{`""`, `"same-module"`} {
		var policy ExecutorPolicy
		if err := json.Unmarshal([]byte(raw), &policy); err != nil {
			t.Errorf("valid executor policy %s: %v", raw, err)
		}
	}
	var decoded struct{ Executor ExecutorPolicy }
	if err := json.Unmarshal([]byte(`{}`), &decoded); err != nil || decoded.Executor != "" {
		t.Fatal("absent executor policy changed its legacy meaning", err)
	}
}
