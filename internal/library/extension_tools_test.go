package library

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/gateway"
)

func TestExtensionToolsHubPreservesRichResultAndPolicies(t *testing.T) {
	hubSandbox(t)
	const spec = "extension:tool.module"
	legacy := fakePluginTools(t, spec)
	if _, err := SetMCPHub(true); err != nil {
		t.Fatal(err)
	}
	if _, err := adoptPluginTools(); err != nil {
		t.Fatal(err)
	}
	name := gateway.ToolPrefix(spec)
	if _, err := change(func(library *Library) error {
		server := library.server(name)
		if server == nil {
			t.Fatal("SDK tool server was not adopted")
		}
		server.Agents = []string{"claude"}
		server.DisabledTools = []string{"crawl"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	previous := runPluginToolResultOf
	calls := 0
	response := json.RawMessage(`{"content":[{"type":"image","data":"aW1hZ2U=","mimeType":"image/png"},{"type":"resource","resource":{"uri":"fixture://document","text":"body","mimeType":"text/plain"}}],"isError":true}`)
	runPluginToolResultOf = func(ctx context.Context, plugin, tool string, args json.RawMessage, agent string) (json.RawMessage, error) {
		calls++
		if plugin != spec || tool != "search" || string(args) != `{"query":"fixture"}` || agent != "claude" {
			t.Errorf("rich SDK callback: %s %s %s %s", plugin, tool, args, agent)
		}
		return bytes.Clone(response), nil
	}
	t.Cleanup(func() { runPluginToolResultOf = previous })
	hub := theHub{}
	result, err := hub.Call(t.Context(), "claude", name, "search", json.RawMessage(`{"query":"fixture"}`))
	if err != nil || !bytes.Equal(result, response) || calls != 1 || len(legacy.ran) != 0 {
		t.Fatalf("hub flattened rich SDK result: %s, %v; rich calls=%d legacy=%v", result, err, calls, legacy.ran)
	}
	if _, err := hub.Call(t.Context(), "codex", name, "search", json.RawMessage(`{}`)); err == nil ||
		!strings.Contains(err.Error(), "isn't switched on for codex") {
		t.Fatalf("SDK callback bypassed agent policy: %v", err)
	}
	if _, err := hub.Call(t.Context(), "claude", name, "crawl", json.RawMessage(`{}`)); err == nil ||
		!strings.Contains(err.Error(), "turned off") {
		t.Fatalf("SDK callback bypassed tool policy: %v", err)
	}
	if calls != 1 || len(legacy.ran) != 0 {
		t.Fatalf("refused hub call reached SDK: rich=%d legacy=%v", calls, legacy.ran)
	}
}

func TestExtensionToolsHubLegacyResultRemainsText(t *testing.T) {
	hubSandbox(t)
	const spec = "legacy-tools"
	legacy := fakePluginTools(t, spec)
	previous := runPluginToolResultOf
	runPluginToolResultOf = func(context.Context, string, string, json.RawMessage, string) (json.RawMessage, error) {
		t.Fatal("legacy plugin reached the SDK rich callback")
		return nil, nil
	}
	t.Cleanup(func() { runPluginToolResultOf = previous })
	result, err := pluginHubCall(t.Context(), "claude", &Server{Plugin: spec}, "search", json.RawMessage(`{"query":"fixture"}`))
	if err != nil || hubText(t, result) != `found {"query":"fixture"}` || len(legacy.ran) != 1 {
		t.Fatalf("legacy result changed: %s, %v; calls=%v", result, err, legacy.ran)
	}
}
