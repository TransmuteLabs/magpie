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
	t.Setenv("SDK_LAUNCH_URL", "own-account")
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
	installed, err := host.Install(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.Find("sdk-start")
	if err != nil {
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
	if _, err := runOut(t, "sdk-start", "run", "hello"); err != nil ||
		!slices.Equal(arguments, []string{"hello"}) || calls != 1 ||
		!slices.Contains(environment, "SDK_LAUNCH_URL=own-account") {
		t.Fatal("disconnected SDK profile did not launch on its own account", arguments, err)
	}
	refuseAfterLookup := func(t *testing.T, change func() error, argument string) {
		t.Helper()
		look := startLook
		defer func() { startLook = look }()
		called := false
		var changeErr error
		startLook = func(bin string) (string, error) {
			path, err := look(bin)
			if err == nil {
				called = true
				changeErr = change()
			}
			return path, err
		}
		before := calls
		_, err := runOut(t, "sdk-start", "run", argument)
		if !called || changeErr != nil {
			t.Fatal("fixture could not change the SDK launch owner during PATH lookup", called, changeErr)
		}
		if err == nil || calls != before {
			t.Fatal("stale SDK launch reached exec after PATH lookup", err)
		}
	}
	t.Run("connected", func(t *testing.T) {
		refuseAfterLookup(t, func() error { _, err := a.ConnectHow(); return err }, "hello")
	})
	before := calls
	out, err := runOut(t, "sdk-start", "env")
	if err != nil || !strings.Contains(out, "active=1\n") || !strings.Contains(out, "SDK_LAUNCH_URL=http://127.0.0.1:") {
		t.Fatal("ordinary env command lost env-only activation", out, err)
	}
	if _, err := runOut(t, "sdk-start", "run", "hello"); err != nil ||
		!slices.Equal(arguments, []string{"--mode", "sdk", "hello"}) || calls != before+1 ||
		!slices.ContainsFunc(environment, func(value string) bool { return strings.HasPrefix(value, "SDK_LAUNCH_URL=http://127.0.0.1:") }) {
		t.Fatal("ordinary CLI did not resolve SDK launch metadata", arguments, err)
	}
	if _, err := runOut(t, "sdk-start", "run", "login"); err != nil ||
		!slices.Equal(arguments, []string{"login"}) || !slices.Contains(environment, "SDK_LAUNCH_URL=own-account") {
		t.Fatal("declared plain command inherited the SDK gateway", arguments, err)
	}
	t.Run("without worker at exec", func(t *testing.T) {
		look := startLook
		defer func() { startLook = look }()
		worker := filepath.Join(host.Namespace(), installed.Directory, entry)
		stopped := filepath.Join(t.TempDir(), entry)
		moved := false
		t.Cleanup(func() {
			if moved {
				if err := os.Rename(stopped, worker); err != nil {
					t.Error(err)
				}
			}
		})
		var stopErr error
		startLook = func(bin string) (string, error) {
			path, err := look(bin)
			if err != nil {
				return path, err
			}
			stopErr = host.Close(t.Context())
			if stopErr == nil {
				stopErr = os.Rename(worker, stopped)
				moved = stopErr == nil
			}
			return path, err
		}
		before := calls
		_, err := runOut(t, "sdk-start", "run", "hello")
		if !moved {
			t.Fatal("fixture worker could not be stopped during PATH lookup", stopErr)
		}
		if err != nil || calls != before+1 {
			t.Fatal("final SDK launch admission needed the worker", err)
		}
	})
	host = extension.Default()
	a, err = agent.Find("sdk-start")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("config", func(t *testing.T) {
		config, err := host.ModuleConfig(t.Context(), manifest.ID)
		if err != nil {
			t.Fatal(err)
		}
		refuseAfterLookup(t, func() error {
			_, err := host.CompareAndSwapModuleConfig(t.Context(), manifest.ID, config.Version, json.RawMessage(`{"changed":true}`))
			return err
		}, "hello")
	})
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	t.Run("replacement", func(t *testing.T) {
		manifest.Version = "1.0.1"
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		refuseAfterLookup(t, func() error { _, err := host.Install(t.Context(), source); return err }, "hello")
	})
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	t.Run("disconnected", func(t *testing.T) {
		refuseAfterLookup(t, a.Disconnect, "hello")
	})
	if _, err := a.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	t.Run("disabled plain", func(t *testing.T) {
		refuseAfterLookup(t, func() error { return host.SetEnabled(t.Context(), manifest.ID, false) }, "login")
	})
	before = calls
	if _, err := runOut(t, "sdk-start", "run", "hello"); err == nil || calls != before {
		t.Fatal("disabled SDK owner silently launched a plain own-account command", err)
	}
}
