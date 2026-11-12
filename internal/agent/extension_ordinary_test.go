package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func installAgentActionFixture(t *testing.T, descriptor sdk.AgentDescriptor) *extension.Manager {
	t.Helper()
	home := t.TempDir()
	for _, env := range []string{"HOME", "USERPROFILE"} {
		t.Setenv(env, home)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "appdata"))
	source := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-p=1", "-o",
		filepath.Join(source, entry), "../extension/testdata/actionworker")
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build action fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{Schema: 1, ID: "fixture.agent", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1}, Capabilities: []sdk.Capability{sdk.AgentWiring}},
		Agents:   []sdk.AgentDescriptor{descriptor}}
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
	if _, err := host.Install(ctx, source); err != nil {
		t.Fatal(err)
	}
	return host
}

func TestExtensionAgentOrdinaryLifecycle(t *testing.T) {
	host := installAgentActionFixture(t, sdk.AgentDescriptor{ID: "fixture-editor", Name: "Fixture editor", Bin: "fixture-editor",
		ConfigFiles: []string{".fixture/config.json"}, Fields: []sdk.AgentField{{Key: "model", Label: "model"}}})
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".fixture", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"model":"own","note":"keep"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	all := All()
	a, err := FindIn(all, "fixture-editor")
	if err != nil || a.Native == nil || a.Field("model") == nil || !a.Detected() {
		t.Fatalf("installed descriptor absent from ordinary All: %+v %v", a, err)
	}
	if _, err := Find("fixture-editor"); err != nil {
		t.Fatal(err)
	}
	if _, err := FindIn(local(), "fixture-editor"); err != nil {
		t.Fatal(err)
	}
	if a.Wired() {
		t.Fatal("unconnected fixture is wired")
	}
	if _, err := a.ConnectHow(); err != nil || !a.Wired() {
		t.Fatalf("ordinary connect: %v", err)
	}
	if err := a.Apply("model", "magpie/test-model"); err != nil || a.Values()["model"] != "magpie/test-model" {
		t.Fatalf("ordinary Apply/Values: %v", err)
	}
	// A new Agent object must retain ownership and the original preimage.
	a, err = Find("fixture-editor")
	if err != nil || !a.Wired() {
		t.Fatalf("ordinary rediscovery lost connection: %v", err)
	}
	before, _ := os.ReadFile(path)
	preview, err := a.Native.Disconnect()
	if err != nil || len(preview.Files) != 1 {
		t.Fatalf("ordinary native disconnect preview: %+v %v", preview, err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("disconnect preview wrote config")
	}
	// Execution rejects edits arriving after preview.
	var values map[string]string
	if err := json.Unmarshal(before, &values); err != nil {
		t.Fatal(err)
	}
	values["note"] = "edited by user"
	edited, _ := json.Marshal(values)
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Native.Execute(preview); err == nil {
		t.Fatal("stale disconnect preview overwrote a user edit")
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	values = nil
	if err := json.Unmarshal(body, &values); err != nil {
		t.Fatal(err)
	}
	if values["model"] != "own" || values["note"] != "edited by user" || values["gateway"] != "" || a.Wired() || !a.Disconnected() {
		t.Fatalf("ordinary disconnect did not restore owned keys: %s", body)
	}
	if err := host.SetEnabled(t.Context(), "fixture.agent", false); err != nil {
		t.Fatal(err)
	}
	if _, err := Find("fixture-editor"); err == nil {
		t.Fatal("disabled extension still registered")
	}
}

func TestExtensionAgentOrdinaryPreservesManualWiring(t *testing.T) {
	installAgentActionFixture(t, sdk.AgentDescriptor{ID: "fixture-editor", Name: "Fixture editor",
		ConfigFiles: []string{".fixture.json"}, Fields: []sdk.AgentField{{Key: "model", Label: "model"}}})
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".fixture.json")
	original := []byte(`{"gateway":"manual","model":"magpie/manual","note":"keep"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Find("fixture-editor")
	if err != nil {
		t.Fatal(err)
	}
	if a.Wired() {
		t.Fatal("manual wiring was adopted without a user action")
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if !bytes.Equal(body, original) {
		t.Fatalf("manual wiring changed: %s", body)
	}
}

func TestExtensionAgentOrdinaryRejectsBuiltinCollision(t *testing.T) {
	installAgentActionFixture(t, sdk.AgentDescriptor{ID: "claude", Name: "Fake Claude",
		ConfigFiles: []string{".fixture.json"}})
	a, err := Find("claude")
	if err != nil || a.Name == "Fake Claude" {
		t.Fatalf("extension replaced a builtin: %+v %v", a, err)
	}
	if PluginErrors()["fixture.agent"] == "" {
		t.Fatal("collision was not reported")
	}
}
