package protocolext

import (
	"encoding/json"
	"testing"
)

func TestContractRefusesUnsupportedAPIBeforeRegistration(t *testing.T) {
	host := InboundContract()
	for _, wire := range []string{
		`{"version":{"major":1,"minor":0},"capabilities":["inbound-codec"]}`,
		`{"version":{"major":0,"minor":2},"capabilities":["inbound-codec"]}`,
		`{"version":{"major":0,"minor":-1},"capabilities":["inbound-codec"]}`,
		`{"version":{"major":0,"minor":1},"capabilities":["outbound-executor"]}`,
		`{"version":{"major":0,"minor":1},"capabilities":["unknown"]}`,
		`{"version":{"major":0,"minor":1},"capabilities":["inbound-codec","inbound-codec"]}`,
		`{"version":{"major":0,"minor":1},"capabilities":[]}`,
	} {
		var module Contract
		if err := json.Unmarshal([]byte(wire), &module); err != nil {
			t.Fatal(err)
		}
		if err := host.Check(module); err == nil {
			t.Errorf("accepted unsupported module: %s", wire)
		}
	}
	if err := host.Check(InboundContract()); err != nil {
		t.Fatal(err)
	}
}

type incompatible struct{ fixture }

func (incompatible) Contract() Contract {
	return Contract{Version: APIVersion{Major: 1}, Capabilities: []Capability{InboundCodec}}
}

func TestRegistryChecksModuleContract(t *testing.T) {
	if _, err := New(incompatible{fixture{"codec", "/rpc"}}); err == nil {
		t.Fatal("incompatible module acquired a route")
	}
}

func TestCatalogContractRequiresNewHostCapability(t *testing.T) {
	if err := InboundContract().Check(CatalogContract()); err == nil {
		t.Fatal("codec-only host accepted the catalog operation")
	}
	if err := HostContract().Check(CatalogContract()); err != nil {
		t.Fatal(err)
	}
	if err := HostContract().Check(InboundContract()); err != nil {
		t.Fatal("catalog host lost existing codec support:", err)
	}
	if !CatalogContract().Has(ModelDiscovery) || InboundContract().Has(ModelDiscovery) {
		t.Fatal("catalog permission is not explicit")
	}
}

type mutableContract struct {
	fixture
	declaration Contract
}

func (module *mutableContract) Contract() Contract { return module.declaration }

func TestRegistryFreezesNegotiatedCapabilitiesAndIdentity(t *testing.T) {
	module := &mutableContract{fixture: fixture{"codec", "/rpc"}, declaration: InboundContract()}
	registry, err := New(module)
	if err != nil {
		t.Fatal(err)
	}
	module.declaration.Capabilities[0] = ModelDiscovery
	module.declaration = CatalogContract()
	module.id, module.path = "changed", "/other"
	entry, found := registry.Lookup("/rpc")
	if !found || entry.Contract().Has(ModelDiscovery) || entry.ID() != "codec" || entry.Path() != "/rpc" {
		t.Fatal("module changed its negotiated identity or capabilities")
	}
	exposed := entry.Contract()
	exposed.Capabilities[0] = ModelDiscovery
	if entry.Contract().Has(ModelDiscovery) || !entry.Contract().Has(InboundCodec) {
		t.Fatal("lookup exposed mutable negotiated capabilities")
	}
}

func TestContractDescriptorsDoNotShareCapabilities(t *testing.T) {
	inbound, catalog, host := InboundContract(), CatalogContract(), HostContract()
	inbound.Capabilities[0], catalog.Capabilities[0], host.Capabilities[0] = AccountAuth, AccountAuth, AccountAuth
	if !InboundContract().Has(InboundCodec) || !CatalogContract().Has(InboundCodec) || HostContract().Has(AccountAuth) {
		t.Fatal("a caller mutated the host contract")
	}
}
