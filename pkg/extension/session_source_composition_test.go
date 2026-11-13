package extension

import (
	"slices"
	"testing"

	"github.com/yetone/magpie/pkg/protocolext"
)

func TestSessionSourceComposesWithAgentAndWindowAuthentication(t *testing.T) {
	manifest := sourceManifest()
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, AgentWiring, AccountAuth, ModelDiscovery)
	manifest.Agents = []AgentDescriptor{{
		ID: "fixture-agent", Name: "Fixture agent", Bin: "fixture-agent",
		ConfigFiles: []string{".fixture/config.json"},
		Launch:      &AgentLaunch{Env: map[string]string{"FIXTURE_GATEWAY": "{gateway}"}},
	}}
	manifest.Providers = []ProviderDescriptor{{ID: "vendor", Name: "Fixture vendor", Protocols: []protocolext.Protocol{"chat"}}}
	manifest.Window = &WindowDescriptor{Metadata: "package.json"}
	manifest.AuthBindings = []WindowAuthBindingDescriptor{{
		ID: "signin", From: "window:page", Provider: "vendor",
		Methods: []WindowAuthMethod{{ID: "browser", Mode: "poll"}},
	}}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{MethodSessionSourceRead, MethodAgentInspect, MethodAgentPlan, MethodAuthStart, MethodAuthFinish} {
		if !slices.Contains(RequiredMethods(manifest), method) {
			t.Fatalf("composed module lost %s", method)
		}
	}
	public := manifest.Public()
	if len(public.SessionSources) != 1 || len(public.Agents) != 1 || len(public.AuthBindings) != 0 || public.Window != nil {
		t.Fatal("source composition changed primary or window-only authority")
	}
	cloned := manifest.Clone()
	cloned.SessionSources[0].Stores[0].HomeRelativePath = ".changed"
	cloned.Agents[0].Launch.Env["FIXTURE_GATEWAY"] = "changed"
	cloned.AuthBindings[0].Methods[0].ID = "changed"
	if manifest.SessionSources[0].Stores[0].HomeRelativePath != ".fixture/sessions" ||
		manifest.Agents[0].Launch.Env["FIXTURE_GATEWAY"] != "{gateway}" ||
		manifest.AuthBindings[0].Methods[0].ID != "browser" {
		t.Fatal("composed source, agent or authentication metadata shared mutable storage")
	}
}
