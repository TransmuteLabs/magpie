package agent

import (
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionAllKnownRetainsDetectedAgent(t *testing.T) {
	descriptor := sdk.AgentDescriptor{
		ID: "fixture-known", Name: "Known fixture", Bin: "fixture-known",
		ConfigFiles: []string{".fixture/config.json"},
		Fields:      []sdk.AgentField{{Key: "model", Label: "Model"}},
	}
	installAgentActionFixture(t, descriptor)
	home, _ := os.UserHomeDir()
	config := filepath.Join(home, ".fixture", "config.json")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(`{"model":"own"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	known, err := FindIn(AllKnown(), descriptor.ID)
	if err != nil || known.extensionMetadata == nil || !known.Detected() {
		t.Fatalf("nonblocking agent discovery lost the installed SDK agent: %+v %v", known, err)
	}
}

func TestExtensionAllKnownRetainsAdoptedProfile(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.Profile = &sdk.AgentProfile{Target: "codex"}
	descriptor.ConfigFiles = []string{".fixture/config.json"}
	descriptor.Fields = []sdk.AgentField{{Key: "model", Label: "Model"}}
	descriptor.Launch.Model = "--model"
	host, home := launchAgentFixture(t, descriptor)
	config := filepath.Join(home, ".fixture", "config.json")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(`{"model":"own"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	builtin, err := FindIn(AllKnown(), "codex")
	if err != nil || builtin.ID != "codex" {
		t.Fatal("an inactive profile displaced the ordinary base", err)
	}
	profile, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	adopted, err := FindIn(AllKnown(), "codex")
	if err != nil || adopted.ID != descriptor.ID {
		t.Fatalf("nonblocking discovery bypassed the adopted profile: %+v %v", adopted, err)
	}
	if err := host.SetEnabled(t.Context(), "fixture.agent", false); err != nil {
		t.Fatal(err)
	}
	disabled, err := FindIn(AllKnown(), "codex")
	if err != nil || disabled.ID != descriptor.ID || disabled.Start().Err == nil {
		t.Fatal("nonblocking discovery returned a built-in while its profile was disabled", err)
	}
	if err := disabled.Disconnect(); err != nil {
		t.Fatal(err)
	}
	released, err := FindIn(AllKnown(), "codex")
	if err != nil || released.ID != "codex" {
		t.Fatal("disconnect did not restore the built-in in nonblocking discovery", err)
	}
}
