package library

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/gwtool"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionToolExposureLibraryContextAndCards(t *testing.T) {
	hubSandbox(t)
	manager := extension.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	manifest := sdk.Manifest{
		Schema: 1, ID: "library.private", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 2}, Capabilities: []sdk.Capability{sdk.ToolProvider}},
		Tools: []sdk.ToolDescriptor{
			{Name: "shared", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "owner-memory", Description: "OWNER_ONLY_DESCRIPTION", Exposure: "local-owner", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	source := t.TempDir()
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "worker"), []byte("metadata fixture; never run"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := extension.NewCatalog(manager.Namespace()).Install(t.Context(), source,
		func(context.Context, extension.Installed) error { return nil }); err != nil {
		t.Fatal(err)
	}
	stop := gwtool.Serve(gwtool.PluginTools)
	t.Cleanup(stop)
	if _, err := change(func(library *Library) error {
		library.MCPHub = true
		library.MCP = append(library.MCP, &Server{
			Name: "sdkprivacy", Plugin: "extension:library.private", Transport: "http", URL: gateway.URL() + gateway.ToolsMCPOne + "library",
			Agents: []string{"claude"},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hub := theHub{}
	owner := extension.WithLocalOwner(t.Context(), true)
	if names := hubNames(hub.Tools(owner, "claude")); strings.Join(names, ",") != "sdkprivacy/owner-memory,sdkprivacy/shared" {
		t.Fatalf("owner's in-process library list lost tools: %v", names)
	}
	// The ordinary configuration card deliberately retains the owner's last
	// inventory; shared HTTP instructions must use the contextual projection.
	if cards := hub.Servers("claude"); len(cards) != 1 || len(cards[0].Tools) != 2 {
		t.Fatalf("fixture did not populate the shared card cache: %+v", cards)
	}
	for _, ctx := range []context.Context{
		t.Context(),
		extension.WithLocalOwner(owner, false),
		gateway.WithHubCaller(t.Context(), gateway.HubCaller{Roots: []string{"file:///owner"}, Client: "owner", Session: "owner"}),
	} {
		hub.Tools(owner, "claude")
		for _, server := range hub.ServersFor(ctx, "claude") {
			for _, tool := range server.Tools {
				if tool.Name == "owner-memory" || strings.Contains(tool.About, "OWNER_ONLY_DESCRIPTION") {
					t.Fatalf("owner's cached card leaked to shared context: %+v", server)
				}
			}
		}
		if names := hubNames(hub.Tools(ctx, "claude")); strings.Join(names, ",") != "sdkprivacy/shared" {
			t.Fatalf("library exposed owner-only tools to an unowned context: %v", names)
		}
		_, err := hub.Call(ctx, "claude", "sdkprivacy", "owner-memory",
			json.RawMessage(`{"localOwner":true,"_meta":{"origin":"local"}}`))
		if err == nil || !strings.Contains(err.Error(), "gives no tool") {
			t.Fatalf("library stale-name fallback bypassed exposure: %v", err)
		}
	}
	if manager.Running(manifest.ID) {
		t.Fatal("denied library calls started the SDK worker")
	}
}
