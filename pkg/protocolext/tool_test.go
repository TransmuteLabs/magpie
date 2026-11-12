package protocolext

import (
	"reflect"
	"testing"
)

func TestToolContractNegotiatesOnlyInboundToolAuthority(t *testing.T) {
	tool := ToolContract()
	if tool.Version != (APIVersion{Major: 0, Minor: 3}) || !reflect.DeepEqual(tool.Capabilities, []Capability{InboundCodec, ToolProvider}) {
		t.Fatalf("wrong tool contract: %+v", tool)
	}
	host := HostContract()
	if host.Version != (APIVersion{Major: 0, Minor: 3}) ||
		!reflect.DeepEqual(host.Capabilities, []Capability{InboundCodec, ModelDiscovery, ToolProvider}) {
		t.Fatalf("legacy host acquired unrelated authority: %+v", host)
	}
	for _, compatible := range []Contract{InboundContract(), CatalogContract(), tool} {
		if err := host.Check(compatible); err != nil {
			t.Fatal("host lost compatible inbound support", err)
		}
	}
	for _, unsupported := range []Contract{
		{Version: APIVersion{Major: 0, Minor: 4}, Capabilities: []Capability{InboundCodec}},
		{Version: APIVersion{Major: 0, Minor: 3}, Capabilities: []Capability{InboundCodec, OutboundExecutor}},
		{Version: APIVersion{Major: 0, Minor: 3}, Capabilities: []Capability{InboundCodec, AccountAuth}},
	} {
		if err := host.Check(unsupported); err == nil {
			t.Fatalf("host accepted unsupported contract: %+v", unsupported)
		}
	}
	if InboundContract().Check(tool) == nil || CatalogContract().Check(tool) == nil || tool.Check(CatalogContract()) == nil {
		t.Fatal("model discovery and tool invocation were implicitly interchangeable")
	}
	if CatalogContract().Version.Minor != 2 || CatalogContract().Has(ToolProvider) {
		t.Fatal("catalog 0.2 compatibility changed")
	}
	tool.Capabilities[1] = AccountAuth
	host.Capabilities[2] = AccountAuth
	if !ToolContract().Has(ToolProvider) || !HostContract().Has(ToolProvider) || HostContract().Has(AccountAuth) {
		t.Fatal("tool capability descriptors share mutable slices")
	}
}

func TestRegistryFreezesNegotiatedToolCapability(t *testing.T) {
	codec := &mutableContract{fixture: fixture{"tool", "/fixture/Control"}, declaration: ToolContract()}
	registry, err := New(codec)
	if err != nil {
		t.Fatal("tool codec was not admitted", err)
	}
	codec.declaration = CatalogContract()
	registered, found := registry.Lookup("/fixture/Control")
	if !found || !registered.Contract().Has(ToolProvider) || registered.Contract().Has(ModelDiscovery) {
		t.Fatal("registered tool authority changed after registration")
	}
	plain := &mutableContract{fixture: fixture{"plain", "/fixture/Plain"}, declaration: CatalogContract()}
	registry, err = New(plain)
	if err != nil {
		t.Fatal(err)
	}
	plain.declaration = ToolContract()
	registered, _ = registry.Lookup("/fixture/Plain")
	if registered.Contract().Has(ToolProvider) {
		t.Fatal("codec gained tool authority after registration")
	}
}
