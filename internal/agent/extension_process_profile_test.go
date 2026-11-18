package agent

import (
	"path/filepath"
	"slices"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestOwnedProcessInheritsProfileConfig(t *testing.T) {
	d := launchAgentDescriptor()
	d.Profile = &sdk.AgentProfile{Target: "opencode"}
	d.Launch.Env["OPENCODE_CONFIG"] = "{config}"
	host, home := launchAgentFixture(t, d)
	a, err := Find(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	s := a.Start()
	if s.Err != nil || !s.Active {
		t.Fatalf("ordinary launch: %+v", s)
	}
	expected := filepath.Join(home, ".config", "opencode", "opencode.json")
	if !slices.Contains(s.Env, "OPENCODE_CONFIG="+expected) {
		t.Fatalf("ordinary config: %+v", s)
	}
	lease, err := host.Acquire(t.Context(), "fixture.agent")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	launch, err := ResolveExtensionProcess(t.Context(), lease, sdk.AgentProcessRecipe{ID: "native", Agent: d.ID, Argv: []string{"app-server"}}, home, 0)
	if err != nil {
		t.Fatalf("ordinary Start resolves %q but owned process rejects same profile: %v", expected, err)
	}
	if !slices.Contains(launch.Env, "OPENCODE_CONFIG="+expected) {
		t.Fatalf("process config: %+v", launch)
	}
}
