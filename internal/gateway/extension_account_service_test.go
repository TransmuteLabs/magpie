package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func TestSDKAccountOnlySearchThroughOrdinaryFront(t *testing.T) {
	directory := buildSDKSearch(t)
	for _, mixed := range []bool{false, true} {
		name := "service"
		if mixed {
			name = "mixed"
		}
		t.Run(name, func(t *testing.T) {
			manager := setupSDKSearch(t, directory, true)
			manifest, err := manager.Manifest("search.fixture")
			if err != nil {
				t.Fatal(err)
			}
			manifest.Contract.Version.Minor = 11
			manifest.Version = "1.1.0"
			manifest.Providers[0].Protocols = nil
			manifest.Contract.Capabilities = slices.DeleteFunc(manifest.Contract.Capabilities, func(capability sdk.Capability) bool {
				return capability == sdk.OutboundExecutor
			})
			if mixed {
				manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.OutboundExecutor, sdk.ModelDiscovery)
				manifest.Providers = append(manifest.Providers, sdk.ProviderDescriptor{
					ID: "inference", Name: "Inference sibling", Protocols: []protocolext.Protocol{protocolext.Chat},
				})
				if _, err := manager.Store().SaveAccount(extension.Account{Plugin: manifest.ID, Provider: "inference", ID: "alice",
					Auth: json.RawMessage(`{"token":"PRIVATE-INFERENCE"}`)}, 0); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Install(t.Context(), directory); err != nil {
				t.Fatal(err)
			}
			trace := filepath.Join(t.TempDir(), "trace")
			configureSDKSearch(t, manager, map[string]any{"trace": trace})
			provider.Changed()
			var service *provider.Provider
			for _, current := range provider.All() {
				scope, ok := current.ExtensionScope()
				if ok && scope.Plugin == manifest.ID && scope.Provider == "vendor" {
					service = &current
				}
			}
			if service == nil {
				t.Fatal("account-only service disappeared from ordinary account selection")
			}
			if service.Chat != "" || service.Anthropic != "" || service.Responses != "" || service.Gemini != "" || service.Decide != "" {
				t.Fatal("service account advertised inference endpoints")
			}
			for _, entry := range provider.Served() {
				if entry.Provider.ID == service.ID {
					t.Fatal("service account invented a routable model")
				}
			}
			server := New()
			reply := readMCP(t, mcpPost(server.Front(), "127.0.0.1:50000",
				`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"service query","count":2}}}`))
			raw, _ = json.Marshal(reply)
			if reply.Error != nil || reply.Result.IsError || !strings.Contains(string(raw), "ordinary search 1.1.0") {
				t.Fatalf("account-only search failed at ordinary Front: %s", raw)
			}
			entries := readSDKSearchTrace(t, trace)
			if len(entries) != 1 || !entries[0].HasAuth || entries[0].Scope.Provider != "vendor" || entries[0].Scope.Account == "" {
				t.Fatalf("service lost selected account authority: %+v", entries)
			}
			scope := entries[0].Scope
			for _, method := range []string{sdk.MethodProviderExecute, sdk.MethodProviderModels} {
				if _, err := manager.Call(t.Context(), manifest.ID, method, scope, sdk.ModelsRequest{}, nil); err == nil {
					t.Fatalf("account-only service acquired %s", method)
				}
			}
			if mixed {
				inference := scope
				inference.Provider = "inference"
				var models sdk.ModelsResult
				if err := manager.Invoke(t.Context(), manifest.ID, sdk.MethodProviderModels, inference, sdk.ModelsRequest{}, &models, nil); err != nil || len(models.Models) != 1 {
					t.Fatalf("service role broke inference sibling: %+v %v", models, err)
				}
			}
			vendor, _ := provider.SearchVendorOf(sdkSearchVendor)
			if _, err := manager.Store().SaveAccount(extension.Account{Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account,
				Auth: json.RawMessage(`{"token":"REPLACED"}`)}, scope.Generation); err != nil {
				t.Fatal(err)
			}
			if _, err := provider.RunExtensionSearch(t.Context(), vendor, service, "stale account", 2); err == nil {
				t.Fatal("revoked service account reached the search action")
			}
			if err := manager.SetEnabled(t.Context(), manifest.ID, false); err != nil {
				t.Fatal(err)
			}
			if _, err := provider.RunExtensionSearch(t.Context(), vendor, service, "disabled", 2); err == nil {
				t.Fatal("disabled service account reached the search action")
			}
			if len(readSDKSearchTrace(t, trace)) != 1 {
				t.Fatal("refused service request reached its author handler")
			}
		})
	}
}
