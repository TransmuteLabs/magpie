package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestOwnedProcessAgentResolutionUsesCurrentBindingAndLiteralArgv(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.Fields = []sdk.AgentField{{Key: "model", Label: "Model"}}
	descriptor.Launch.Model = "--model"
	host, home := launchAgentFixture(t, descriptor)
	owned, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	recipe := sdk.AgentProcessRecipe{ID: "native", Agent: descriptor.ID, Argv: []string{"app-server", "--stdio", `';$(never-run)`}}
	lease, err := host.Acquire(t.Context(), "fixture.agent")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, err := ResolveExtensionProcess(t.Context(), lease, recipe, home, 73); err == nil {
		t.Fatal("a disconnected profile granted process authority")
	}
	if _, err := owned.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	launch, err := ResolveExtensionProcess(t.Context(), lease, recipe, home, 73)
	if err != nil {
		t.Fatal("current ordinary agent binding could not resolve its process", err)
	}
	start := owned.Start()
	expected := append(slices.Clone(start.Args), start.Flag, start.Model)
	expected = append(expected, recipe.Argv...)
	if !slices.Equal(launch.Recipe.Argv, expected) || launch.Executable != filepath.Join(home, "bin", descriptor.Bin) ||
		launch.Profile != descriptor.ID || launch.RouteID != 73 || launch.Revision == "" {
		t.Fatalf("process launch changed the ordinary binding or literal arguments: %+v", launch)
	}
	for _, variable := range start.Env {
		if !slices.Contains(launch.Env, variable) {
			t.Fatal("process launch dropped an existing host-owned launch variable")
		}
	}
	if err := owned.Field("model").Set("magpie/changed"); err != nil {
		t.Fatal(err)
	}
	if err := launch.Check(t.Context()); err == nil {
		t.Fatal("a retained process grant adopted a newer profile model")
	}
}

func TestOwnedProcessAgentResolutionRejectsChangedOwnedFiles(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.ConfigFiles = []string{".owned-process/config.json"}
	descriptor.Launch.Args = map[string]string{"--config": "{config}"}
	host, home := launchAgentFixture(t, descriptor)
	owned, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owned.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	lease, err := host.Acquire(t.Context(), "fixture.agent")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	launch, err := ResolveExtensionProcess(t.Context(), lease,
		sdk.AgentProcessRecipe{ID: "native", Agent: descriptor.ID, Argv: []string{"app-server", "--stdio"}}, home, 74)
	if err != nil {
		t.Fatal(err)
	}
	release, err := launch.Admit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := os.WriteFile(filepath.Join(home, ".owned-process/config.json"), []byte(`{"personal":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := launch.Check(context.Background()); err == nil {
		t.Fatal("changed file-plan content retained process authority")
	}
}
