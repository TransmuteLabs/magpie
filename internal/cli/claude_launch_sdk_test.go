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
	"testing"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestClaudeRunRespectsSDKOwner(t *testing.T) {
	home := helpHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_PROFILE_URL", "old")
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Anthropic: "https://example.test", Key: "k",
		Models: []string{"claude-opus-4-6", "glm-4.6"}}); err != nil {
		t.Fatal(err)
	}
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
	config := ".fixture claude/settings {config}.json"
	manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: "fixture.claude-profile", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 5}, Capabilities: []sdk.Capability{sdk.AgentWiring}},
		Agents: []sdk.AgentDescriptor{
			{ID: "claude-fixture", Name: "Claude fixture", Profile: &sdk.AgentProfile{Target: "claude"},
				ConfigFiles: []string{config}, Fields: []sdk.AgentField{{Key: "model", Label: "Model"}},
				Launch: &sdk.AgentLaunch{Env: map[string]string{"CLAUDE_PROFILE_URL": "{url}"},
					Args: map[string]string{"--settings": "{config}"}, Model: "--model"}},
			{ID: "claude-wiring-only", Name: "Claude wiring only", Profile: &sdk.AgentProfile{Target: "claude"},
				ConfigFiles: []string{".fixture claude/wiring.json"}},
		},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := extension.Default().Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := extension.Default().Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	find := func(id string) *agent.Agent {
		t.Helper()
		selected, err := agent.Find(id)
		if err != nil {
			t.Fatal(err)
		}
		return selected
	}
	connect := func(id string) *agent.Agent {
		t.Helper()
		selected := find(id)
		if _, err := selected.ConnectHow(); err != nil {
			t.Fatal(err)
		}
		return selected
	}
	connect("claude-fixture")

	oldExec, oldLook, oldStart := startExec, startLook, startUp
	oldFind, oldNative := claudeFind, claudeRunExec
	oldGateway, oldLetGo := claudeGatewayUp, claudeLetGo
	t.Cleanup(func() {
		startExec, startLook, startUp = oldExec, oldLook, oldStart
		claudeFind, claudeRunExec = oldFind, oldNative
		claudeGatewayUp, claudeLetGo = oldGateway, oldLetGo
	})
	builtinStarts := 0
	startUp = func() { builtinStarts++ }
	claudeLetGo = func() {}
	claudeGatewayUp = func() bool { return true }
	claudeFind = func() string { return filepath.Join(home, "claude") }
	nativeCalls, sdkCalls := 0, 0
	claudeRunExec = func(_ string, _, _ []string) error {
		nativeCalls++
		return nil
	}
	startLook = func(bin string) (string, error) {
		if bin != "claude" {
			return "", errors.New("unexpected executable")
		}
		return filepath.Join(home, bin), nil
	}
	var arguments, environment []string
	startExec = func(_, _ string, args, env []string) error {
		sdkCalls++
		arguments, environment = slices.Clone(args), slices.Clone(env)
		return exitError{code: 17}
	}
	run := func(alias string, input, want []string) {
		t.Helper()
		var exit exitError
		before := sdkCalls
		_, err := runOut(t, append([]string{alias, "run"}, input...)...)
		want = append([]string{"--settings", filepath.Join(home, filepath.FromSlash(config))}, want...)
		if !errors.As(err, &exit) || exit.code != 17 || sdkCalls != before+1 || nativeCalls != 0 || builtinStarts != 0 ||
			!slices.Equal(arguments, want) || !slices.Contains(environment, "CLAUDE_PROFILE_URL="+gateway.URL()) ||
			slices.Contains(environment, "CLAUDE_PROFILE_URL=old") {
			t.Fatalf("Claude %s launch: SDK=%d native=%d args=%q err=%v", alias, sdkCalls, nativeCalls, arguments, err)
		}
	}
	refuse := func(alias string) {
		t.Helper()
		before, nativeBefore := sdkCalls, nativeCalls
		var exit exitError
		if _, err := runOut(t, alias, "run"); err == nil || errors.As(err, &exit) || sdkCalls != before || nativeCalls != nativeBefore {
			t.Fatalf("Claude %s launched with unavailable ownership: SDK=%d native=%d err=%v", alias, sdkCalls, nativeCalls, err)
		}
	}

	run("claude", []string{"--help"}, []string{"--model", "magpie/test-model", "--help"})
	for _, input := range [][]string{{"-p", "keep 'quotes' and {config}", "--model", "sonnet"}, {"--model=opus"}} {
		run("cc", input, input)
	}
	if _, err := os.Stat(agent.ClaudeLaunchPath()); !os.IsNotExist(err) {
		t.Fatalf("SDK launch touched native overlay: %v", err)
	}
	look := startLook
	startLook = func(bin string) (string, error) {
		if err := extension.Default().SetEnabled(t.Context(), manifest.ID, false); err != nil {
			return "", err
		}
		return look(bin)
	}
	refuse("claude")
	startLook = look
	entries, err := extension.Default().List()
	if err != nil || len(entries) != 1 || !entries[0].Disabled {
		t.Fatalf("late-disable fixture did not disable its owner: %v, %v", entries, err)
	}
	refuse("claude-code")
	if err := extension.Default().SetEnabled(t.Context(), manifest.ID, true); err != nil {
		t.Fatal(err)
	}
	moduleConfig, err := extension.Default().ModuleConfig(t.Context(), manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extension.Default().CompareAndSwapModuleConfig(t.Context(), manifest.ID, moduleConfig.Version, json.RawMessage(`{"updated":true}`)); err != nil {
		t.Fatal(err)
	}
	refuse("claude")
	if err := find("claude-fixture").Sync(); err != nil {
		t.Fatal(err)
	}
	run("claude", nil, []string{"--model", "magpie/test-model"})
	if err := find("claude-fixture").Disconnect(); err != nil {
		t.Fatal(err)
	}
	wiring := connect("claude-wiring-only")
	refuse("cc")
	if err := wiring.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if _, err := runOut(t, "claude", "run"); err != nil || nativeCalls != 1 || builtinStarts != 1 {
		t.Fatalf("Claude did not recover native launch after disconnect: native=%d err=%v", nativeCalls, err)
	}

	user := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(user), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(user, []byte(`{"model":"opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	adopted := `{"model":"relay/glm-4.6"}`
	claudeRunExec = func(_ string, _, _ []string) error {
		nativeCalls++
		selected, err := agent.Find("claude-fixture")
		if err != nil {
			return err
		}
		if _, err := selected.ConnectHow(); err != nil {
			return err
		}
		return os.WriteFile(user, []byte(adopted), 0o600)
	}
	if _, err := runOut(t, "claude", "run"); err != nil || nativeCalls != 2 {
		t.Fatalf("native launch did not allow adoption while its child ran: native=%d err=%v", nativeCalls, err)
	}
	if raw, err := os.ReadFile(user); err != nil || string(raw) != adopted {
		t.Fatalf("native launch rewrote the new SDK owner's settings: %q (%v)", raw, err)
	}
	ledger := filepath.Join(filepath.Dir(provider.Path()), "sdk-agent-bindings.json")
	if err := os.WriteFile(ledger, []byte(`{"schema":1,"records":`), 0o600); err != nil {
		t.Fatal(err)
	}
	refuse("claude")
}
