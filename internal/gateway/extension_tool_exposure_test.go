package gateway

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func exposureGatewayFixture(t *testing.T) (*extensionToolAccountFake, *atomic.Int64) {
	t.Helper()
	fake := extensionToolGatewayFixture(t)
	fake.entries[0].Manifest.Contract.Version = sdk.APIVersion{Major: 1, Minor: 2}
	fake.entries[0].Manifest.Tools = []sdk.ToolDescriptor{
		{Name: "legacy", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "public", Exposure: "shared", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "owner-memory", Description: "OWNER_ONLY_DESCRIPTION", Exposure: "local-owner", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	source := t.TempDir()
	body, err := json.Marshal(fake.entries[0].Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "worker"), []byte("metadata only; must never execute"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := extension.NewCatalog(extension.Default().Namespace()).Install(t.Context(), source,
		func(context.Context, extension.Installed) error { return nil }); err != nil {
		t.Fatal(err)
	}
	oldList, oldRun := pluginTools, runExtensionPluginTool
	pluginTools = plugin.Tools
	calls := &atomic.Int64{}
	runExtensionPluginTool = func(ctx context.Context, spec, name string, args json.RawMessage) (sdk.ToolResult, error) {
		if name == "owner-memory" && !extension.LocalOwner(ctx) {
			t.Error("unauthorized request reached the SDK runtime")
		}
		calls.Add(1)
		return sdk.ToolResult{Content: json.RawMessage(`[{"type":"text","text":"ran SDK tool"}]`)}, nil
	}
	t.Cleanup(func() { pluginTools, runExtensionPluginTool = oldList, oldRun })
	return fake, calls
}

// The bridge uses the same contextual gateway entry points as Library's
// in-process plugin adapter, including its stale-name call path.
type exposureGatewayHub struct{ spec string }

func (hub exposureGatewayHub) On() bool { return true }
func (hub exposureGatewayHub) Servers(string) []HubServer {
	return []HubServer{{Name: "sdk", Known: true, Tools: []HubToolNote{{Name: "owner-memory", About: "OWNER_ONLY_DESCRIPTION"}}}}
}
func (hub exposureGatewayHub) ServersFor(ctx context.Context, agent string) []HubServer {
	servers := hub.Servers(agent)
	servers[0].Tools = nil
	tools, _ := PluginToolsOf(ctx, hub.spec)
	for _, tool := range tools {
		servers[0].Tools = append(servers[0].Tools, HubToolNote{Name: tool.Name, About: tool.Description})
	}
	return servers
}
func (hub exposureGatewayHub) Tools(ctx context.Context, _ string) []HubTool {
	tools, _ := PluginToolsOf(ctx, hub.spec)
	var out []HubTool
	for _, tool := range tools {
		out = append(out, HubTool{Server: "sdk", Tool: tool.Name, Spec: hubSpec(tool.Name)})
	}
	return out
}
func (hub exposureGatewayHub) Call(ctx context.Context, agent, _, tool string, args json.RawMessage) (json.RawMessage, error) {
	return RunPluginToolResultOf(ctx, hub.spec, tool, args, agent)
}
func (exposureGatewayHub) Stop()  {}
func (exposureGatewayHub) Prune() {}

func TestExtensionToolExposureHTTPProvenance(t *testing.T) {
	_, calls := exposureGatewayFixture(t)
	_, keys := newCaller(t, "named remote client")
	if err := access.ConfigureLAN(true, false); err != nil {
		t.Fatal(err)
	}
	mine := access.LANSecret()
	if mine == "" || mine == keys[0] {
		t.Fatal("fixture has no distinct default gateway key")
	}
	t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
	oldOwn, oldNets := ownAddr, WSLNets
	ownAddr = func(address netip.Addr) bool { return address == netip.MustParseAddr("192.168.50.7") }
	_, wsl, _ := net.ParseCIDR("172.28.80.1/20")
	WSLNets = func() []*net.IPNet { return []*net.IPNet{wsl} }
	t.Cleanup(func() { ownAddr, WSLNets = oldOwn, oldNets })
	oldHub := MCPHub
	MCPHub = exposureGatewayHub{spec: "extension:tool.module"}
	t.Cleanup(func() { MCPHub = oldHub })
	handler := lanGuard(New().Handler())
	for _, test := range []struct {
		name, peer, key, header, value, trust string
		owner, page                           bool
	}{
		{name: "direct-owner", peer: here, owner: true},
		{name: "ipv6-owner", peer: "[::1]:50000", owner: true},
		{name: "node-fetch-cors", peer: here, header: "Sec-Fetch-Mode", value: "cors", owner: true},
		{name: "default-loopback", peer: here, key: mine, owner: true},
		{name: "default-own-address", peer: "192.168.50.7:50000", key: mine, owner: true},
		{name: "default-wsl", peer: "172.28.80.9:50000", key: mine, owner: true},
		{name: "named-loopback", peer: here, key: keys[0]},
		{name: "named-remote", peer: "10.0.0.8:50000", key: keys[0]},
		{name: "default-remote", peer: "10.0.0.8:50000", key: mine},
		{name: "forwarded", peer: here, key: mine, header: "X-Forwarded-For", value: "10.0.0.8"},
		{name: "trusted-forwarded", peer: here, key: mine, header: "Forwarded", value: "for=10.0.0.8", trust: "1"},
		{name: "browser-origin", peer: here, key: mine, header: "Origin", value: "http://localhost:5173"},
		{name: "browser-user-agent", peer: here, key: mine, header: "User-Agent", value: "Mozilla/5.0"},
		{name: "page-port", peer: here, key: mine, page: true},
		{name: "unknown-peer", peer: "unparseable", key: mine},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("MAGPIE_TRUST_PROXY", test.trust)
			for _, route := range []struct{ path, private, public string }{
				{ToolsMCPPath, ToolPrefix("extension:tool.module") + "_owner-memory", ToolPrefix("extension:tool.module") + "_legacy"},
				{ToolsMCPOne + ToolPrefix("extension:tool.module"), "owner-memory", "legacy"},
				{HubMCPAll, "sdk__owner-memory", "sdk__legacy"},
			} {
				post := func(body string) *httptest.ResponseRecorder {
					request := httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(body))
					request.RemoteAddr, request.Host = test.peer, "127.0.0.1:3425"
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("X-Magpie-Origin", "local")
					request.Header.Set("X-Magpie-Local-Owner", "true")
					if test.key != "" {
						request.Header.Set("Authorization", "Bearer "+test.key)
					}
					if test.header != "" {
						request.Header.Set(test.header, test.value)
					}
					// Even an inherited trusted bit is overwritten at HTTP entry.
					request = request.WithContext(extension.WithLocalOwner(extension.WithCallerID(request.Context(), "owner"), true))
					if test.page {
						request = FromPagePort(request)
					}
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					return response
				}
				list := post(toolsList)
				if test.owner && (list.Code != http.StatusOK || !strings.Contains(list.Body.String(), route.private)) {
					t.Fatalf("owner did not list %s: %d %s", route.path, list.Code, list.Body)
				}
				if !test.owner && (strings.Contains(list.Body.String(), "owner-memory") || strings.Contains(list.Body.String(), "OWNER_ONLY_DESCRIPTION")) {
					t.Fatalf("shared list exposed owner tool at %s: %d %s", route.path, list.Code, list.Body)
				}
				before := calls.Load()
				body, _ := json.Marshal(map[string]any{
					"jsonrpc": "2.0", "id": 7, "method": "tools/call",
					"params": map[string]any{"name": route.private,
						"arguments": map[string]any{"origin": "local", "localOwner": true, "_meta": map[string]any{"owner": true}},
						"_meta":     map[string]any{"origin": "local", "localOwner": true, HubRootsMeta: []string{"file:///owner"}, HubSessionMeta: "owner"}},
				})
				response := post(string(body))
				if test.owner {
					if response.Code != http.StatusOK || calls.Load() != before+1 || !strings.Contains(response.Body.String(), "ran SDK tool") {
						t.Fatalf("owner call failed at %s: %d %s", route.path, response.Code, response.Body)
					}
				} else if calls.Load() != before {
					t.Fatalf("forged owner call reached runtime at %s: %d %s", route.path, response.Code, response.Body)
				}
				if list.Code == http.StatusOK {
					before = calls.Load()
					shared := post(toolsCall(route.public, `{}`))
					if shared.Code != http.StatusOK || calls.Load() != before+1 {
						t.Fatalf("default shared tool changed at %s: %d %s", route.path, shared.Code, shared.Body)
					}
				}
				if route.path == HubMCPAll && !test.owner {
					initial := post(`{"jsonrpc":"2.0","id":8,"method":"initialize","params":{}}`)
					if strings.Contains(initial.Body.String(), "owner-memory") || strings.Contains(initial.Body.String(), "OWNER_ONLY_DESCRIPTION") {
						t.Fatalf("cached hub metadata exposed an owner tool: %s", initial.Body)
					}
				}
			}
		})
	}
}

func TestExtensionToolExposureHTTPManifestTransition(t *testing.T) {
	fake, calls := exposureGatewayFixture(t)
	_, keys := newCaller(t, "remote client")
	t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
	handler := lanGuard(New().Handler())
	name := ToolPrefix("extension:tool.module") + "_public"
	post := func(body string) *httptest.ResponseRecorder {
		return hubPostAt(handler, ToolsMCPPath, "10.0.0.8:50000", body, "Authorization", "Bearer "+keys[0])
	}
	if response := post(toolsList); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), name) {
		t.Fatalf("shared SDK tool was not listed: %d %s", response.Code, response.Body)
	}
	fake.entries[0].Manifest.Tools[1].Exposure = "local-owner"
	source := t.TempDir()
	body, err := json.Marshal(fake.entries[0].Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "worker"), []byte("updated metadata; never run"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := extension.NewCatalog(extension.Default().Namespace()).Install(t.Context(), source,
		func(context.Context, extension.Installed) error { return nil }); err != nil {
		t.Fatal(err)
	}
	response := post(toolsCall(name, `{"origin":"local","_meta":{"localOwner":true}}`))
	if calls.Load() != 0 || strings.Contains(response.Body.String(), "ran SDK tool") {
		t.Fatalf("cached shared name bypassed the new manifest: %d %s", response.Code, response.Body)
	}
	if response := post(toolsList); strings.Contains(response.Body.String(), name) {
		t.Fatalf("new owner policy did not revoke the shared list: %s", response.Body)
	}
}

func TestExtensionToolExposureNativeControlListing(t *testing.T) {
	exposureGatewayFixture(t)
	server := New()
	server.extensions = extension.Default()
	inbound := fixtureInbound{
		id:       "tool.module:control",
		contract: sdk.Contract{Version: sdk.APIVersion{Major: 1}, Capabilities: []sdk.Capability{sdk.ToolProvider}},
	}
	body, err := json.Marshal(sdk.ToolRequest{ID: "owner-memory", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		peer  string
		owner bool
	}{{here, true}, {"10.0.0.8:50000", false}, {"unparseable", false}} {
		request := httptest.NewRequest(http.MethodPost, "/native/control", nil)
		request.RemoteAddr = test.peer
		request = request.WithContext(extension.WithLocalOwner(request.Context(), true))
		result, err := server.protocolToolBody(request, inbound, body)
		if (err == nil) != test.owner || (len(result) != 0) != test.owner {
			t.Fatalf("native control peer=%s result=%s err=%v", test.peer, result, err)
		}
	}
}
