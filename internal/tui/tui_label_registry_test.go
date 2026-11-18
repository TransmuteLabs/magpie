package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func installTUILabelFixture(t *testing.T, takeover bool) *agent.Agent {
	t.Helper()
	home := t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE"} {
		t.Setenv(name, home)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "appdata"))
	t.Setenv("SHELL", "/bin/bash")
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "fixture-tui"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	directory := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	ctx, cancel := context.WithTimeout(testenv.DeadlineContext(t, 2*time.Second), time.Minute)
	defer cancel()
	build := proc.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-p=1",
		"-o", filepath.Join(directory, entry), "../extension/testdata/actionworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	build.WaitDelay = 2 * time.Second
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build existing SDK label fixture: %v\n%s", err, output)
	}
	descriptor := sdk.AgentDescriptor{
		ID: "fixture-tui-label", Name: "TUI label fixture", Bin: "fixture-tui",
		ConfigFiles: []string{".fixture/config.json"},
		Fields:      []sdk.AgentField{{Key: "model", Label: "Fixture worker pool"}},
	}
	if takeover {
		descriptor.Profile = &sdk.AgentProfile{Target: "codex"}
		descriptor.Launch = &sdk.AgentLaunch{Env: map[string]string{"FIXTURE_URL": "{url}", "FIXTURE_KEY": "{key}"}}
	}
	manifest := sdk.Manifest{Schema: 1, ID: "fixture.tui-label", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 5}, Capabilities: []sdk.Capability{sdk.AgentWiring}},
		Agents:   []sdk.AgentDescriptor{descriptor}}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), body, 0o600); err != nil {
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
	if _, err := host.Install(ctx, directory); err != nil {
		t.Fatal(err)
	}
	row, err := agent.Find(descriptor.ID)
	if err != nil || len(row.Fields) != 1 || row.Fields[0].Key != "model" || row.Fields[0].Label != descriptor.Fields[0].Label {
		t.Fatalf("ordinary installed SDK fields: agent=%+v error=%v", row, err)
	}
	if _, _, owned := row.SDKLibraryTarget(); !owned {
		t.Fatal("ordinary row lost its actual SDK ownership metadata")
	}
	if takeover {
		path := filepath.Join(home, ".fixture/config.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"model":"personal"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := row.ConnectHow(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := row.Disconnect(); err != nil {
				t.Error(err)
			}
		})
		row, err = agent.Find("codex")
		if err != nil || row.ID != descriptor.ID {
			t.Fatalf("ordinary SDK takeover was not active: agent=%+v error=%v", row, err)
		}
	}
	if !slices.ContainsFunc(agent.All(), func(candidate *agent.Agent) bool { return candidate.ID == row.ID }) {
		t.Fatal("ordinary All omitted the installed SDK row")
	}
	return row
}

func TestTUIStaticGuardAllowsInstalledSDK(t *testing.T) {
	installTUILabelFixture(t, false)
	t.Run("UnchangedWordOracle", TestEveryWordHasZh)
}

func TestTUIStaticGuardPreservesAdoptedNativeBase(t *testing.T) {
	profile := installTUILabelFixture(t, true)
	base, _, owned := profile.SDKLibraryTarget()
	if !owned || base == nil || base == profile || base.ID != "codex" {
		t.Fatal("actual SDK takeover did not retain its native base metadata")
	}
	words, _ := tuiWords(t)
	for _, field := range base.Fields {
		if !keepAsIs[field.Label] && !slices.Contains(words, field.Label) {
			t.Fatalf("native base label disappeared after actual takeover: %s/%s %q", base.ID, field.Key, field.Label)
		}
	}
	t.Run("UnchangedWordOracle", TestEveryWordHasZh)
}
