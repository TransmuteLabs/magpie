package extension

import (
	"encoding/json"
	"testing"

	"github.com/yetone/magpie/pkg/protocolext"
)

func accountOperationManifest() Manifest {
	manifest := testManifest(InboundCodec, OutboundExecutor, AgentWiring, AccountAuth, ModuleRPC, NativeAccountOperations)
	manifest.Contract.Version.Minor = 6
	manifest.Inbounds = []InboundDescriptor{{ID: "native", Path: "/fixture.Service/Read"}}
	manifest.Providers = []ProviderDescriptor{providerDescriptor()}
	manifest.Agents = []AgentDescriptor{{ID: "agent", Name: "Agent", Bin: "agent",
		Fields: []AgentField{{Key: "model", Label: "Model"}},
		Launch: &AgentLaunch{Model: "--model"}}}
	manifest.Actions = []ActionDescriptor{{ID: "read", Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	manifest.AccountOperations = []AccountOperationDescriptor{{ID: "read", Agent: "agent", Provider: "fixture", Action: "read"}}
	return manifest
}

func TestNativeAccountOperationFiniteAuthority(t *testing.T) {
	manifest := accountOperationManifest()
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	snapshot := manifest.Public()
	snapshot.AccountOperations[0].Agent = "other"
	if manifest.AccountOperations[0].Agent != "agent" {
		t.Fatal("native operation declarations share mutable authority")
	}
	for _, name := range []string{"old-floor", "no-capability", "foreign-agent", "foreign-provider",
		"foreign-action", "duplicate", "no-model", "service-action", "no-declarations"} {
		t.Run(name, func(t *testing.T) {
			value := manifest.Clone()
			switch name {
			case "old-floor":
				value.Contract.Version.Minor = 5
			case "no-capability":
				value.Contract.Capabilities = value.Contract.Capabilities[:len(value.Contract.Capabilities)-1]
			case "foreign-agent":
				value.AccountOperations[0].Agent = "other"
			case "foreign-provider":
				value.AccountOperations[0].Provider = "other"
			case "foreign-action":
				value.AccountOperations[0].Action = "other"
			case "duplicate":
				value.AccountOperations = append(value.AccountOperations, value.AccountOperations[0])
			case "no-model":
				value.Agents[0].Launch.Model = ""
			case "service-action":
				value.Actions[0].Component = "sidecar"
			case "no-declarations":
				value.AccountOperations = nil
			}
			if value.Validate() == nil {
				t.Fatal("native account operation accepted ambiguous or unnegotiated authority")
			}
		})
	}
	if protocolext.HostContract().Check(protocolext.AccountOperationContract()) != nil ||
		protocolext.ValidateExecutor(protocolext.SameModuleExecutor, protocolext.AccountOperation) != nil {
		t.Fatal("native account operation cannot bind its original executor")
	}
	legacy := manifest.Contract
	legacy.Version.Minor = 5
	if FullContract().Check(legacy) == nil {
		t.Fatal("accepted SDK 1.5 acquired a new native account capability")
	}
}

func TestNativeAccountOperationIdentityUsesModuleIDGrammar(t *testing.T) {
	for _, identity := range []string{"read.more-v1", "Read", "read more", "read\u00a0more", "read\ufeffmore", "прочитать", "read/more", ""} {
		t.Run(identity, func(t *testing.T) {
			manifest := accountOperationManifest()
			manifest.AccountOperations[0].ID = identity
			err := manifest.Validate()
			if (identity == "read.more-v1") != (err == nil) {
				t.Fatalf("native account operation identity %q: %v", identity, err)
			}
		})
	}
}
