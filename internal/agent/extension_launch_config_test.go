package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/agentplug"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func sdkLaunchConfigEntry(t *testing.T, filename string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("actual entry fixture is a POSIX shell executable")
	}
	body := "#!/bin/sh\nprintf 'CONFIG=%s\\n' \"$OPENCODE_CONFIG\"\n/bin/cat \"$OPENCODE_CONFIG\" || exit 91\nprintf '\\n'\nprintf 'ARG=%s\\n' \"$@\"\n"
	writeFile(t, filename, body)
	if err := os.Chmod(filename, 0o700); err != nil {
		t.Fatal(err)
	}
}

func sdkLaunchConfigRun(t *testing.T, start Starting, config string) {
	t.Helper()
	if start.Err != nil || !start.Active {
		t.Fatalf("declared SDK environment did not resolve its exact current configuration path: %+v", start)
	}
	command := exec.Command(start.Bin, append(start.Args, "--probe")...)
	command.Env = append(os.Environ(), start.Env...)
	output, err := command.CombinedOutput()
	if err != nil || !slices.Contains(start.Env, "OPENCODE_CONFIG="+config) ||
		!strings.Contains(string(output), "CONFIG="+config+"\n") ||
		!strings.Contains(string(output), "ARG=--probe\n") {
		t.Fatalf("real declared entry could not read its configuration: %v\n%s", err, output)
	}
}

func TestSDKLaunchConfigEnvironmentStartsRealEntryWithSpacesAndLiteralTokens(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.ConfigFiles = []string{".fixture config/settings {key} with spaces.json"}
	descriptor.Fields = []sdk.AgentField{{Key: "model", Label: "Model"}}
	descriptor.Launch.Model = "--model"
	descriptor.Launch.Env["OPENCODE_CONFIG"] = "{config}"
	descriptor.Launch.Args["--settings"] = "{config}"
	_, home := launchAgentFixture(t, descriptor)
	config := filepath.Join(home, filepath.FromSlash(descriptor.ConfigFiles[0]))
	writeFile(t, config, `{"model":"own","personal":"kept"}`)
	sdkLaunchConfigEntry(t, filepath.Join(home, "bin", descriptor.Bin))
	a, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	a, err = Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	start := a.Start()
	sdkLaunchConfigRun(t, start, config)
	if !slices.Equal(start.Args, []string{"--mode", "sdk", "--settings", config}) {
		t.Fatal("config argument and environment resolved different paths:", start.Args)
	}
	if raw, err := os.ReadFile(config); err != nil || !strings.Contains(string(raw), `"personal":"kept"`) {
		t.Fatal("SDK launch lost user configuration content:", err)
	}
}

func TestSDKLaunchConfigProfileUsesCurrentBaseEnvironmentPath(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.Profile = &sdk.AgentProfile{Target: "opencode"}
	descriptor.Launch.Env["OPENCODE_CONFIG"] = "{config}"
	_, home := launchAgentFixture(t, descriptor)
	writeFile(t, filepath.Join(home, "bin", "opencode"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(home, "bin", "opencode"), 0o700); err != nil {
		t.Fatal(err)
	}
	sdkLaunchConfigEntry(t, filepath.Join(home, "bin", descriptor.Bin))
	first := filepath.Join(home, "first config", "opencode.json")
	second := filepath.Join(home, "next config", "opencode.json")
	writeFile(t, first, `{"personal":"first"}`)
	writeFile(t, second, `{"personal":"second"}`)
	t.Setenv("OPENCODE_CONFIG_DIR", filepath.Dir(first))
	a, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	sdkLaunchConfigRun(t, a.Start(), first)
	t.Setenv("OPENCODE_CONFIG_DIR", filepath.Dir(second))
	a, err = Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	sdkLaunchConfigRun(t, a.Start(), second)
}

func TestSDKLaunchConfigRejectsProfileWithoutAnAvailableConfigPath(t *testing.T) {
	descriptor := launchAgentDescriptor()
	descriptor.Profile = &sdk.AgentProfile{Target: "fixture-base"}
	descriptor.Launch.Env["OPENCODE_CONFIG"] = "{config}"
	host := installAgentActionFixture(t, descriptor)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	start := extensionControlledAgent(home, host, ExtensionAgent{Plugin: "fixture.agent", Agent: descriptor},
		&Agent{ID: "fixture-base", Bin: descriptor.Bin}).Start()
	if start.Err == nil || !strings.Contains(start.Err.Error(), "configuration path") {
		t.Fatalf("a profile without a config path could emit an empty/literal config environment: %+v", start)
	}
}

func TestSDKLaunchConfigKeepsLegacyEnvironmentTemplateSemantics(t *testing.T) {
	config := filepath.Join(t.TempDir(), "legacy config.json")
	legacy := newLaunch(agentplug.Desc{ID: "legacy", Bin: "legacy",
		Launch: &agentplug.Launch{Env: map[string]string{"CONFIG": "{config}", "URL": "{url}"}}}, config)
	if !slices.Equal(legacy.vars("http://gateway", "key"), []string{"CONFIG={config}", "URL=http://gateway"}) {
		t.Fatal("SDK config environment support changed legacy agentplug placeholders")
	}
}
