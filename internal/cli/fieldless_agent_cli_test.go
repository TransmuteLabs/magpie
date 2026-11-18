package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func fieldlessCLIAgent(t *testing.T) *agent.Agent {
	t.Helper()
	home := helpHome(t)
	source := t.TempDir()
	manifest := sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: "fixture.fieldless-cli", Version: "1.0.0",
		Runtime: "exec", Entry: "worker",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 5}, Capabilities: []sdk.Capability{sdk.AgentWiring}},
		Agents: []sdk.AgentDescriptor{{
			ID: "fieldless-sdk", Name: "Fieldless SDK",
			Profile:     &sdk.AgentProfile{Target: "codex"},
			ConfigFiles: []string{".sdk-fieldless/settings.json"},
		}},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, manifest.Entry), []byte("fixture, never executed"), 0o700); err != nil {
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
	if _, err := extension.NewCatalog(host.Namespace()).Install(t.Context(), source, func(context.Context, extension.Installed) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".sdk-fieldless", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"user\":\"preserved\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := agent.Find("fieldless-sdk")
	if err != nil || resolved == nil || resolved.Native == nil || len(resolved.Fields) != 0 {
		t.Fatalf("ordinary SDK discovery did not produce a configuration-only native agent: %+v %v", resolved, err)
	}
	return resolved
}

func runFieldlessCommand(args ...string) (err error) {
	defer func() {
		if failure := recover(); failure != nil {
			err = fmt.Errorf("agent command panicked: %v", failure)
		}
	}()
	return run(args)
}

func TestFieldlessSDKAgentCLIRejectsModelValuesWithoutPanic(t *testing.T) {
	resolved := fieldlessCLIAgent(t)
	before, err := os.ReadFile(resolved.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"connect", "model-that-does-not-exist", "status"} {
		if err := runFieldlessCommand(resolved.ID, value); err == nil || !strings.Contains(err.Error(), "no configurable fields") {
			t.Errorf("bare value %q did not fail through the ordinary CLI field contract: %v", value, err)
		}
	}
	if err := runFieldlessCommand(resolved.ID, "--verbose"); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("flag was treated as a field: %v", err)
	}
	after, err := os.ReadFile(resolved.Path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("rejected commands changed the agent configuration: %q %v", after, err)
	}
}

func TestFieldlessSDKAgentCLIDefaultIsIdempotentBeforeConnection(t *testing.T) {
	resolved := fieldlessCLIAgent(t)
	before, err := os.ReadFile(resolved.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"default", "DEFAULT"} {
		if err := runFieldlessCommand(resolved.ID, word); err != nil {
			t.Errorf("native disconnect on an already disconnected agent failed: %v", err)
		}
	}
	after, err := os.ReadFile(resolved.Path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("idempotent disconnect changed the untouched agent configuration: %q %v", after, err)
	}
	if resolved.Native.Read().Provider != "disconnected" {
		t.Fatal("idempotent disconnect connected the native agent")
	}
}

func TestFieldlessNativeAgentHelpOffersCheckedDisconnect(t *testing.T) {
	resolved := fieldlessCLIAgent(t)
	help := agentHelp(resolved)
	if !strings.Contains(help, "magpie "+resolved.ID+" default") || strings.Contains(help, "<model>") {
		t.Fatalf("configuration-only native help hides disconnect or offers a nonexistent model: %s", help)
	}
}
