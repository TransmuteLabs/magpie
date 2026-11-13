package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSDKAgentLaunchCLIUsesOrdinaryStarterAndRefusesDisabledOwner(t *testing.T) {
	helpHome(t)
	source := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin/go"), "build", "-mod=readonly", "-p=1", "-o",
		filepath.Join(source, entry), "../extension/testdata/actionworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: "fixture.cli-agent", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 5}, Capabilities: []sdk.Capability{sdk.AgentWiring}},
		Agents: []sdk.AgentDescriptor{{ID: "sdk-launch", Name: "SDK launch", Bin: "sdk-launch", Aliases: []string{"sdk-start"},
			Launch: &sdk.AgentLaunch{Env: map[string]string{"SDK_LAUNCH_URL": "{url}"}, Args: map[string]string{"--mode": "sdk"}, Plain: []string{"login"}}}},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	host := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := host.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := host.Install(ctx, source); err != nil {
		t.Fatal(err)
	}
	a, err := agent.Find("sdk-start")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	oldExec, oldLook := startExec, startLook
	t.Cleanup(func() { startExec, startLook = oldExec, oldLook })
	startLook = func(bin string) (string, error) {
		if bin != "sdk-launch" {
			return "", errors.New("unexpected executable")
		}
		return filepath.Join(source, bin), nil
	}
	calls := 0
	var arguments, environment []string
	startExec = func(_, _ string, args, env []string) error {
		calls++
		arguments, environment = slices.Clone(args), slices.Clone(env)
		return nil
	}
	out, err := runOut(t, "sdk-start", "env")
	if err != nil || !strings.Contains(out, "active=1\n") || !strings.Contains(out, "SDK_LAUNCH_URL=http://127.0.0.1:") {
		t.Fatal("ordinary env command lost env-only activation", out, err)
	}
	if _, err := runOut(t, "sdk-start", "run", "hello"); err != nil ||
		!slices.Equal(arguments, []string{"--mode", "sdk", "hello"}) || calls != 1 ||
		!slices.ContainsFunc(environment, func(value string) bool { return strings.HasPrefix(value, "SDK_LAUNCH_URL=http://127.0.0.1:") }) {
		t.Fatal("ordinary CLI did not resolve SDK launch metadata", arguments, err)
	}
	if _, err := runOut(t, "sdk-start", "run", "login"); err != nil ||
		!slices.Equal(arguments, []string{"login"}) || slices.ContainsFunc(environment, func(value string) bool { return strings.HasPrefix(value, "SDK_LAUNCH_URL=") }) {
		t.Fatal("declared plain command inherited the SDK gateway", arguments, err)
	}
	if err := host.SetEnabled(t.Context(), "fixture.cli-agent", false); err != nil {
		t.Fatal(err)
	}
	before := calls
	if _, err := runOut(t, "sdk-start", "run", "hello"); err == nil || calls != before {
		t.Fatal("disabled SDK owner silently launched a plain own-account command", err)
	}
}
