package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/gwtool"
	"github.com/yetone/magpie/internal/library"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func sdkInstalledToolsFixture(t *testing.T) (*extension.Manager, http.Handler) {
	t.Helper()
	home := sandboxHome(t)
	t.Setenv("MAGPIE_BUN", filepath.Join(home, "no-bun"))
	t.Setenv("MAGPIE_NO_ACCOUNTS", "1")
	t.Setenv("MAGPIE_PLUGIN_MARKET", "off")
	t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
	bin := filepath.Join(home, "bin")
	testenv.Program(t, filepath.Join(bin, "claude"), "#!/bin/sh\n")
	if runtime.GOOS == "windows" {
		if err := os.WriteFile(filepath.Join(bin, "claude.exe"), nil, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	claude := filepath.Join(home, ".claude")
	if err := os.Mkdir(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := extension.Default()
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	catalog := extension.NewCatalog(manager.Namespace())
	for _, manifest := range []sdk.Manifest{
		{ID: "minimax", Tools: []sdk.ToolDescriptor{{Name: "minimax_generate_image", Description: "Generate an image"}}},
		{ID: "zcode", Tools: []sdk.ToolDescriptor{{Name: "zcode_account_status", Description: "Inspect local account status", Exposure: "local-owner"}}},
	} {
		manifest.Schema, manifest.Version, manifest.Runtime, manifest.Entry = sdk.ManifestSchema, "1.0.0", "exec", entry
		manifest.Contract = sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 2}, Capabilities: []sdk.Capability{sdk.ToolProvider}}
		manifest.Tools[0].InputSchema = json.RawMessage(`{"type":"object"}`)
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		source := t.TempDir()
		if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, entry), []byte("metadata fixture; never executed"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.Install(t.Context(), source, func(context.Context, extension.Installed) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	handler := Handler(nil, nil)
	if _, err := library.Sync(); err != nil {
		t.Fatal(err)
	}
	return manager, handler
}

func TestSDKInstalledToolsUseLibrarySwitch(t *testing.T) {
	manager, desktop := sdkInstalledToolsFixture(t)
	browser := webPageHandler(t, appWebHost{}, nil)
	list := func(handler http.Handler) map[string]pluginEntryJSON {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/plugins", nil)
		request.Header.Set("X-Magpie-Local-Owner", "true")
		request = request.WithContext(extension.WithLocalOwner(t.Context(), true))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var state pluginsJSON
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &state) != nil || state.SDKError != "" {
			t.Fatalf("GET /api/plugins: %d %s", response.Code, response.Body)
		}
		rows := make(map[string]pluginEntryJSON)
		for _, row := range state.Plugins {
			rows[row.Spec] = row
		}
		return rows
	}
	flip := func(name string, on bool) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"name": name, "on": on})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/library/servers/plugin-on", strings.NewReader(string(raw))).WithContext(t.Context())
		response := httptest.NewRecorder()
		desktop.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("POST /api/library/servers/plugin-on: %d %s", response.Code, response.Body)
		}
	}
	check := func(rows map[string]pluginEntryJSON, id, tool, description string, on bool) {
		t.Helper()
		row := rows["sdk:"+id]
		spec := "extension:" + id
		if row.SDK == nil || row.Off || row.ToolsServer == nil || row.ToolsServer.Plugin != spec ||
			row.ToolsServer.Name != "extension-"+id || row.ToolsServer.On != on {
			t.Errorf("%s does not reflect the Library switch: off=%v server=%+v", id, row.Off, row.ToolsServer)
		}
		want := pluginToolJSON{Name: tool, Description: description, MCP: "extension-" + id + "_" + tool}
		if len(row.Tools) != 1 || row.Tools[0] != want {
			t.Errorf("%s tools = %+v, want %+v", id, row.Tools, want)
		}
	}
	checkMiniMax := func(rows map[string]pluginEntryJSON, on bool) {
		t.Helper()
		check(rows, "minimax", "minimax_generate_image", "Generate an image", on)
	}
	checkZCode := func(rows map[string]pluginEntryJSON, on bool) {
		t.Helper()
		check(rows, "zcode", "zcode_account_status", "Inspect local account status", on)
	}

	for _, id := range []string{"minimax", "zcode"} {
		if !library.PluginServers()["extension:"+id].On {
			t.Fatalf("ordinary Library sync did not register %s", id)
		}
	}
	flip("extension:minimax", false)
	rows := list(desktop)
	checkMiniMax(rows, false)
	checkZCode(rows, true)
	flip("extension:minimax", true)
	flip("extension:zcode", false)
	rows = list(desktop)
	checkMiniMax(rows, true)
	checkZCode(rows, false)
	flip("extension:zcode", true)
	checkZCode(list(desktop), true)

	rows = list(browser)
	checkMiniMax(rows, true)
	if row := rows["sdk:zcode"]; row.SDK == nil || len(row.Tools) != 0 || row.ToolsServer != nil {
		t.Fatalf("browser listing exposed local-owner tools: %+v", row.Tools)
	}

	config := settings.Load()
	config.LAN = true
	if err := settings.Save(config); err != nil {
		t.Fatal(err)
	}
	key, err := access.Update("add-key", access.Change{Name: "SDK tools shared fixture"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gwtool.Serve(gwtool.PluginTools))
	front := gateway.New().Front()
	for _, own := range []bool{true, false} {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3425"+gateway.ToolsMCPPath,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)).WithContext(t.Context())
		request.RemoteAddr = "127.0.0.1:34567"
		request.Header.Set("Content-Type", "application/json")
		if !own {
			request.RemoteAddr = "192.0.2.5:34567"
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response := httptest.NewRecorder()
		front.ServeHTTP(response, request)
		body := response.Body.String()
		if response.Code != http.StatusOK || !strings.Contains(body, "extension-minimax_minimax_generate_image") ||
			strings.Contains(body, "extension-zcode_zcode_account_status") != own {
			t.Fatalf("MCP discovery with local owner = %v after giving both servers: %d %s", own, response.Code, body)
		}
	}

	if err := manager.SetEnabled(t.Context(), "zcode", false); err != nil {
		t.Fatal(err)
	}
	if row := list(desktop)["sdk:zcode"]; !row.Off || len(row.Tools) != 0 || row.ToolsServer != nil {
		t.Fatalf("disabled module advertised tools: off=%v tools=%+v server=%+v", row.Off, row.Tools, row.ToolsServer)
	}
	if manager.Running("minimax") || manager.Running("zcode") || plugin.Running() {
		t.Fatal("metadata reads or Library switches warmed a plugin runtime")
	}
}
