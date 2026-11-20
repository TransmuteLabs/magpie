package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSDKProfileRevealUsesLaunchConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX launcher")
	}
	home := sandboxHome(t)
	bin := filepath.Join(home, "bin")
	testenv.Program(t, filepath.Join(bin, "opencode"), "#!/bin/sh\nexit 0\n")
	testenv.Program(t, filepath.Join(home, ".local", "bin", "zsh"), "#!/bin/sh\nexit 1\n")
	t.Setenv("PATH", bin)
	t.Setenv("SHELL", "/bin/bash")
	proc.Forget()
	t.Cleanup(proc.Forget)
	config := filepath.Join(home, "custom config", "opencode.json")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(`{"personal":"kept"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_CONFIG_DIR", filepath.Dir(config))

	const moduleID, profileID = "fixture.agent-reveal", "opencode-sdk-reveal"
	source := t.TempDir()
	cmd := exec.CommandContext(testenv.DeadlineContext(t, time.Minute),
		filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-p=1",
		"-o", filepath.Join(source, "worker"), "../extension/testdata/actionworker")
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build SDK profile fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{
		Schema: 1, ID: moduleID, Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 5}, Capabilities: []sdk.Capability{sdk.AgentWiring}},
		Agents: []sdk.AgentDescriptor{{
			ID: profileID, Name: "OpenCode reveal fixture", Profile: &sdk.AgentProfile{Target: "opencode"},
			Launch: &sdk.AgentLaunch{Env: map[string]string{"OPENCODE_CONFIG": "{config}"}},
		}},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), body, 0o600); err != nil {
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
	if _, err := host.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	post := func(w Windows, id string) *httptest.ResponseRecorder {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"agent": id})
		rec := httptest.NewRecorder()
		Handler(w, nil).ServeHTTP(rec, httptest.NewRequest("POST", "/api/agents/reveal", strings.NewReader(string(b))))
		return rec
	}
	profile, err := agent.Find(profileID)
	if err != nil {
		t.Fatal(err)
	}
	var shown []string
	desktop := revealHost{shown: &shown}
	if rec := post(desktop, profileID); rec.Code == http.StatusNoContent || len(shown) != 0 {
		t.Fatalf("profile before adoption revealed inherited settings: %d, %q", rec.Code, shown)
	}
	if _, err := profile.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	profile, err = agent.Find("opencode")
	if err != nil || profile.ID != profileID {
		t.Fatalf("OpenCode did not resolve to its adopted SDK profile: %v", err)
	}
	start := profile.Start()
	if start.Err != nil || !start.Active || !slices.Contains(start.Env, "OPENCODE_CONFIG="+config) {
		t.Fatalf("ordinary start did not resolve the expected config: %+v", start)
	}
	if profile.Path != "" {
		t.Fatalf("launch-only profile declares a writable path: %q", profile.Path)
	}
	rows := state().Agents
	index := slices.IndexFunc(rows, func(row agentJSON) bool { return row.ID == profileID })
	if index < 0 {
		t.Fatal("the Agents page lost the adopted SDK profile")
	}
	if row := rows[index]; row.Path != tilde(config) {
		t.Errorf("Agents row shows %q, launch uses %q", row.Path, tilde(config))
	}
	for _, id := range []string{"opencode", config} {
		rec := post(desktop, id)
		if rec.Code == http.StatusNoContent || len(shown) != 0 {
			t.Fatalf("non-row ID %q revealed a file: %d, %q", id, rec.Code, shown)
		}
	}
	if rec := post(webTab{}, profileID); rec.Code == http.StatusNoContent {
		t.Fatalf("browser revealed the SDK settings file: %d", rec.Code)
	}
	if rec := post(desktop, profileID); rec.Code != http.StatusNoContent || !slices.Equal(shown, []string{config}) {
		t.Errorf("SDK profile reveal: %d %s, showed %q, want %q", rec.Code, rec.Body, shown, config)
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	shown = nil
	if rec := post(desktop, profileID); rec.Code != http.StatusNoContent || !slices.Equal(shown, []string{filepath.Dir(config)}) {
		t.Errorf("SDK profile reveal with missing settings: %d %s, showed %q", rec.Code, rec.Body, shown)
	}
}

func TestAgentRevealLeavesStoppedWSLDistroAsleep(t *testing.T) {
	sandboxHome(t)
	const distro, id = "Ubuntu", "claude@wsl:Ubuntu"
	root := t.TempDir()
	config := filepath.Join(root, "home", "me", ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agent.FakeWSL(map[string]string{distro: "home:/home/me\ndir:.claude\nbin:claude\n"},
		map[string]string{distro: root}))
	var shown []string
	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		Handler(revealHost{shown: &shown}, nil).ServeHTTP(rec,
			httptest.NewRequest("POST", "/api/agents/reveal", strings.NewReader(`{"agent":"`+id+`"}`)))
		return rec
	}
	if rec := post(); rec.Code != http.StatusNoContent || !slices.Equal(shown, []string{config}) {
		t.Fatalf("running WSL agent reveal: %d %s, showed %q, want %q", rec.Code, rec.Body, shown, config)
	}
	agent.StopFakeWSL(distro)
	before := agent.FakeWSLRuns(distro)
	shown = nil
	if rec := post(); rec.Code == http.StatusNoContent || len(shown) != 0 || agent.FakeWSLRuns(distro) != before {
		t.Fatalf("reveal woke or opened settings of a stopped WSL agent: %d %s, showed %q, calls %d to %d",
			rec.Code, rec.Body, shown, before, agent.FakeWSLRuns(distro))
	}
}
