package extension

import (
	"context"
	"encoding/json"
	"testing"
)

func accountProviderManifest() Manifest {
	manifest := testManifest(AccountAuth, ToolProvider)
	manifest.Contract.Version.Minor = 11
	manifest.Providers = []ProviderDescriptor{{ID: "service", Name: "Service"}}
	manifest.Tools = []ToolDescriptor{{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	return manifest
}

func TestAccountProviderHasNoInventedInferenceCapability(t *testing.T) {
	manifest := accountProviderManifest()
	if err := manifest.Validate(); err != nil {
		t.Fatalf("account-backed tool service must not need a fabricated inference provider: %v", err)
	}
	for minor := 0; minor <= 10; minor++ {
		legacy := manifest.Clone()
		legacy.Contract.Version.Minor = minor
		if err := legacy.Validate(); err == nil {
			t.Errorf("API 1.%d acquired unnegotiated account-only semantics", minor)
		}
	}
	for _, change := range []func(*Manifest){
		func(m *Manifest) { m.Contract.Capabilities = []Capability{ToolProvider} },
		func(m *Manifest) { m.Providers[0].Models = providerDescriptor().Models },
		func(m *Manifest) { m.Providers[0].Protocols = providerDescriptor().Protocols },
		func(m *Manifest) { m.Contract.Capabilities = append(m.Contract.Capabilities, OutboundExecutor) },
		func(m *Manifest) { m.Contract.Capabilities = append(m.Contract.Capabilities, ModelDiscovery) },
	} {
		invalid := manifest.Clone()
		change(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Error("account-only service accepted undeclared inference authority")
		}
	}
}

func TestMixedAccountProviderRejectsInferenceBeforeAuthorHandler(t *testing.T) {
	manifest := accountProviderManifest()
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, OutboundExecutor, ModelDiscovery)
	manifest.Providers = append(manifest.Providers, providerDescriptor())
	calls := 0
	module := Module{Manifest: manifest, Handlers: map[string]Handler{}}
	for _, method := range RequiredMethods(manifest) {
		module.Handlers[method] = func(context.Context, Call, *Stream) (any, error) {
			calls++
			return nil, nil
		}
	}
	handlers, _, err := module.prepare()
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{MethodProviderModels, MethodProviderExecute} {
		call := Call{Scope: Scope{Provider: "service"}, Params: json.RawMessage(`{"protocol":"chat","model":"invented"}`)}
		if _, err := handlers[method](t.Context(), call, nil); err == nil {
			t.Errorf("%s reached an account-only service", method)
		}
		if calls != 0 {
			t.Fatal("inference handler ran for an account-only descriptor")
		}
		call.Scope.Provider = "fixture"
		if _, err := handlers[method](t.Context(), call, nil); err != nil {
			t.Fatalf("inference sibling was rejected: %v", err)
		}
		if calls != 1 {
			t.Fatal("inference sibling did not reach its handler")
		}
		calls = 0
	}
	if _, err := handlers[MethodToolCall](t.Context(), Call{Scope: Scope{Provider: "service"}, Params: json.RawMessage(`{"id":"search","arguments":{}}`)}, nil); err != nil {
		t.Fatalf("service tool rejected: %v", err)
	}
	if calls != 1 {
		t.Fatal("service tool did not reach its handler")
	}
}

func TestAccountServiceSearchDoesNotPromiseInferenceTasks(t *testing.T) {
	manifest := searchManifest()
	manifest.Contract.Version.Minor = 11
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, AccountAuth, UsageReporter)
	manifest.Providers = []ProviderDescriptor{{ID: "service", Name: "Service"}}
	manifest.SearchSources[0].Provider = "service"
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	manifest.Providers[0].AccountTasks = []AccountTaskDescriptor{{ID: "refresh-index", Action: "index"}}
	manifest.Actions = append(manifest.Actions, ActionDescriptor{ID: "index", Effect: "write", InputSchema: json.RawMessage(`{"type":"object"}`)})
	for _, mixed := range []bool{false, true} {
		value := manifest.Clone()
		if mixed {
			value.Contract.Capabilities = append(value.Contract.Capabilities, OutboundExecutor, ModelDiscovery)
			value.Providers = append(value.Providers, providerDescriptor())
		}
		if err := value.Validate(); err == nil {
			t.Errorf("account-only provider promised an inference task (mixed=%v)", mixed)
		}
	}
}
