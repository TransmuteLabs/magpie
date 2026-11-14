package extension

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestSDK16SourceAndNativeAccountAuthorityComposeWithoutServiceDelegation(t *testing.T) {
	manifest := accountOperationManifest()
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, SessionSource, ModelDiscovery)
	manifest.SessionSources = sourceManifest().SessionSources
	manifest.Window = &WindowDescriptor{Metadata: "package.json"}
	manifest.AuthBindings = []WindowAuthBindingDescriptor{{
		ID: "signin", From: "window:page", Provider: "fixture",
		Methods: []WindowAuthMethod{{ID: "browser", Mode: "poll"}},
	}}
	manifest.Services = []ServiceDescriptor{{
		ID: "helper", Runtime: "exec", Entry: "helper",
		Contract: ServiceContract{ID: "fixture-helper", Version: 1},
		Methods:  []string{"helper-read"}, Start: "lazy",
	}}
	manifest.Actions = append(manifest.Actions, ActionDescriptor{
		ID: "helper-read", Component: "helper", Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`),
	})
	if err := manifest.Validate(); err != nil {
		t.Fatal("composed SDK 1.6 module was refused", err)
	}
	for _, method := range []string{MethodSessionSourceRead, MethodInboundDecode, MethodModuleCall, MethodAgentInspect, MethodAuthStart} {
		if !slices.Contains(RequiredMethods(manifest), method) {
			t.Fatalf("composed module lost %s", method)
		}
	}
	public := manifest.Public()
	if len(public.SessionSources) != 1 || len(public.AccountOperations) != 1 ||
		len(public.Actions) != 1 || public.Actions[0].ID != "read" ||
		public.Window != nil || len(public.AuthBindings) != 0 || len(public.Services) != 0 {
		t.Fatal("primary handshake lost its contracts or acquired private packaging authority")
	}
	cloned := manifest.Clone()
	cloned.AccountOperations[0].Agent = "changed"
	cloned.SessionSources[0].Stores[0].HomeRelativePath = ".changed"
	if manifest.AccountOperations[0].Agent != "agent" ||
		manifest.SessionSources[0].Stores[0].HomeRelativePath != ".fixture/sessions" {
		t.Fatal("composed declarations share mutable authority")
	}
	service, err := ServiceManifest(manifest, "helper")
	if err != nil {
		t.Fatal(err)
	}
	if len(service.SessionSources) != 0 || len(service.AccountOperations) != 0 ||
		len(service.Agents) != 0 || len(service.Providers) != 0 ||
		len(service.Contract.Capabilities) != 1 || !service.Contract.Has(ModuleRPC) {
		t.Fatal("private service inherited source, agent or account authority")
	}
	for _, missing := range []Capability{SessionSource, NativeAccountOperations} {
		changed := manifest.Clone()
		changed.Contract.Capabilities = slices.DeleteFunc(changed.Contract.Capabilities, func(capability Capability) bool {
			return capability == missing
		})
		if changed.Validate() == nil {
			t.Fatalf("composed descriptors bypassed missing %s negotiation", missing)
		}
	}
	legacy := manifest.Clone()
	legacy.Contract.Version.Minor = 5
	if legacy.Validate() == nil {
		t.Fatal("SDK 1.5 acquired source or native account authority")
	}
}
