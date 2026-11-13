package extension

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/yetone/magpie/pkg/protocolext"
)

func testManifest(capabilities ...Capability) Manifest {
	return Manifest{
		Schema: 1, ID: "fixture", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract: Contract{Version: APIVersion{Major: 1}, Capabilities: capabilities},
	}
}

func providerDescriptor() ProviderDescriptor {
	return ProviderDescriptor{ID: "fixture", Name: "Fixture", Protocols: []protocolext.Protocol{protocolext.Chat},
		Models: []Model{{ID: "model", Name: "Model", Protocols: []protocolext.Protocol{protocolext.Chat}}}}
}

func completeManifest() Manifest {
	manifest := testManifest(InboundCodec, OutboundExecutor, AgentWiring, AccountAuth, ModelDiscovery,
		UsageReporter, OperationControl, ToolProvider, SessionMigration)
	manifest.Inbounds = []InboundDescriptor{{ID: "codec", Path: "/fixture.Service/Call"}}
	manifest.Providers = []ProviderDescriptor{providerDescriptor()}
	manifest.Agents = []AgentDescriptor{{ID: "agent", Name: "Agent"}}
	manifest.Sessions = []SessionDescriptor{{ID: "format", Name: "Format"}}
	manifest.Tools = []ToolDescriptor{{Name: "tool", Description: "Fixture tool", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	return manifest
}

func TestManifestRequiresValidNegotiatedDescriptors(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Manifest)
	}{
		{"schema", func(manifest *Manifest) { manifest.Schema = 2 }},
		{"id", func(manifest *Manifest) { manifest.ID = "../bad" }},
		{"version", func(manifest *Manifest) { manifest.Version = "01.0.0" }},
		{"runtime", func(manifest *Manifest) { manifest.Runtime = "shared-library" }},
		{"absolute entry", func(manifest *Manifest) { manifest.Entry = "/worker" }},
		{"traversal entry", func(manifest *Manifest) { manifest.Entry = "../worker" }},
		{"windows entry", func(manifest *Manifest) { manifest.Entry = `C:\worker` }},
		{"empty entry", func(manifest *Manifest) { manifest.Entry = "" }},
		{"contract version", func(manifest *Manifest) { manifest.Contract.Version.Major = 2 }},
		{"unknown capability", func(manifest *Manifest) {
			manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, "unknown")
		}},
		{"duplicate capability", func(manifest *Manifest) {
			manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, InboundCodec)
		}},
		{"duplicate inbound id", func(manifest *Manifest) { manifest.Inbounds = append(manifest.Inbounds, manifest.Inbounds[0]) }},
		{"duplicate inbound path", func(manifest *Manifest) {
			manifest.Inbounds = append(manifest.Inbounds, InboundDescriptor{ID: "another", Path: manifest.Inbounds[0].Path})
		}},
		{"reserved inbound", func(manifest *Manifest) { manifest.Inbounds[0].Path = "/api/settings" }},
		{"core inbound", func(manifest *Manifest) { manifest.Inbounds[0].Path = "/v1/messages" }},
		{"escaped inbound", func(manifest *Manifest) { manifest.Inbounds[0].Path = "/fixture/%63all" }},
		{"duplicate provider", func(manifest *Manifest) { manifest.Providers = append(manifest.Providers, manifest.Providers[0]) }},
		{"unknown protocol", func(manifest *Manifest) { manifest.Providers[0].Protocols[0] = "unknown" }},
		{"duplicate model", func(manifest *Manifest) {
			manifest.Providers[0].Models = append(manifest.Providers[0].Models, manifest.Providers[0].Models[0])
		}},
		{"unadvertised model protocol", func(manifest *Manifest) { manifest.Providers[0].Models[0].Protocols[0] = "gemini" }},
		{"duplicate agent", func(manifest *Manifest) { manifest.Agents = append(manifest.Agents, manifest.Agents[0]) }},
		{"duplicate format", func(manifest *Manifest) { manifest.Sessions = append(manifest.Sessions, manifest.Sessions[0]) }},
		{"duplicate tool", func(manifest *Manifest) { manifest.Tools = append(manifest.Tools, manifest.Tools[0]) }},
		{"invalid schema", func(manifest *Manifest) { manifest.Tools[0].InputSchema = json.RawMessage(`"not a schema"`) }},
		{"missing provider", func(manifest *Manifest) { manifest.Providers = nil }},
		{"missing agent", func(manifest *Manifest) { manifest.Agents = nil }},
		{"missing format", func(manifest *Manifest) { manifest.Sessions = nil }},
		{"missing tool", func(manifest *Manifest) { manifest.Tools = nil }},
		{"missing inbound", func(manifest *Manifest) { manifest.Inbounds = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := completeManifest()
			test.change(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	if err := completeManifest().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.0.0", "0.1.2-dev.1+build.01", "123.0.42-rc.2"} {
		manifest := testManifest(OperationControl)
		manifest.Version = version
		if err := manifest.Validate(); err != nil {
			t.Fatal(version, err)
		}
	}
	for _, version := range []string{"v1.0.0", "1.0", "1.0.0-", "1.0.0+", "1.0.0-01", "1.0.0+bad+build"} {
		manifest := testManifest(OperationControl)
		manifest.Version = version
		if err := manifest.Validate(); err == nil {
			t.Fatal("accepted invalid semver", version)
		}
	}
}

func TestManifestPublicFreezesAuthorityAndClearsOnlyPackaging(t *testing.T) {
	manifest := completeManifest()
	manifest.Args = []string{"--stdio"}
	public := manifest.Public()
	if public.Runtime != "" || public.Entry != "" || public.Args != nil {
		t.Fatal("public manifest retained packaging")
	}
	expected := cloneManifest(manifest)
	expected.Runtime, expected.Entry, expected.Args = "", "", nil
	if !reflect.DeepEqual(public, expected) {
		t.Fatal("public manifest changed authority")
	}
	manifest.Contract.Capabilities[0] = AccountAuth
	manifest.Providers[0].Protocols[0] = "gemini"
	manifest.Providers[0].Models[0].Protocols[0] = "gemini"
	manifest.Tools[0].InputSchema[0] = '['
	if !reflect.DeepEqual(public, expected) {
		t.Fatal("public manifest aliases mutable declarations")
	}
}

func TestModuleRequiresEveryAdvertisedHandlerAndRefusesUnknownMethods(t *testing.T) {
	manifest := completeManifest()
	module := Module{Manifest: manifest, Handlers: map[string]Handler{}}
	if err := module.Validate(); err == nil {
		t.Fatal("module advertised unimplemented methods")
	}
	for _, method := range RequiredMethods(manifest) {
		module.Handlers[method] = testNoop
		capability, found := MethodCapability(method)
		if !found || !manifest.Contract.Has(capability) {
			t.Fatal("method lost its capability", method)
		}
	}
	if err := module.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"private.admin", MethodDescribe} {
		module.Handlers[method] = testNoop
		if err := module.Validate(); err == nil {
			t.Fatal("unnegotiated method registered", method)
		}
		delete(module.Handlers, method)
	}
	if _, found := MethodCapability(MethodDescribe); found {
		t.Fatal("describe acquired a capability")
	}
	module.Manifest.Contract.Capabilities = []Capability{InboundCodec, ModelDiscovery}
	module.Manifest.Providers, module.Manifest.Agents, module.Manifest.Sessions, module.Manifest.Tools = nil, nil, nil, nil
	module.Handlers = map[string]Handler{MethodInboundDecode: testNoop, MethodInboundResponse: testNoop}
	if err := module.Validate(); err != nil {
		t.Fatal("inbound-only model projection refused", err)
	}
	if len(RequiredMethods(module.Manifest)) != 2 {
		t.Fatal("inbound model projection required provider.models")
	}
	legacy := protocolext.HostContract()
	if legacy.Version.Major != 0 || legacy.Version.Minor != 3 || protocolext.CatalogContract().Version.Minor != 2 ||
		FullContract().Version.Major != 1 || FullContract().Version.Minor != 2 {
		t.Fatal("external version altered the legacy codec contract")
	}
}

func TestFrameUsesStableJSONAndBase64Bodies(t *testing.T) {
	frame := Frame{Kind: KindCall, ID: "call", Parent: "parent", Method: MethodProviderExecute,
		Scope: Scope{Plugin: "fixture", Account: "account", Generation: 7, Attempt: 9},
		Auth:  json.RawMessage(`{"secret":"selected"}`), Deadline: 123456,
		Params: json.RawMessage(`{"body":"AAEC"}`),
	}
	body, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{`"generation":7`, `"attempt":9`, `"deadline":123456`, `"parent":"parent"`, `"auth":{"secret":"selected"}`} {
		if !strings.Contains(string(body), text) {
			t.Fatalf("missing %s in %s", text, body)
		}
	}
	var request NativeRequest
	if err := json.Unmarshal(frame.Params, &request); err != nil || !reflect.DeepEqual(request.Body, []byte{0, 1, 2}) {
		t.Fatal("binary fields stopped using base64", err)
	}
}
