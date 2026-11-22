package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func TestSDKPluginListingUsesFrozenPresentation(t *testing.T) {
	sandboxHome(t)
	t.Setenv("MAGPIE_PLUGIN_MARKET", "off")
	t.Setenv("MAGPIE_BUN", "")
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	host := extension.Default()
	t.Cleanup(func() {
		if err := host.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})

	manifest := sdkGUIManifest("1.0.0")
	manifest.ID = "list-suite"
	if runtime.GOOS == "windows" {
		manifest.Entry += ".exe"
	}
	manifest.Window = &sdk.WindowDescriptor{Metadata: "ui/package.json"}
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.AccountAuth, sdk.ProviderSignIn)
	manifest.Providers = []sdk.ProviderDescriptor{
		{ID: "primary", Name: "Example Go", Protocols: []protocolext.Protocol{protocolext.Chat}},
		{ID: "secondary", Name: "Example Zen", Protocols: []protocolext.Protocol{protocolext.Chat}},
	}
	for _, descriptor := range manifest.Providers {
		manifest.AuthBindings = append(manifest.AuthBindings, sdk.WindowAuthBindingDescriptor{
			ID: descriptor.ID, From: sdk.ProviderSignInPart, Provider: descriptor.ID,
			Methods: []sdk.WindowAuthMethod{{ID: "device", Mode: "poll", Label: "Device sign-in"}},
		})
	}
	source := t.TempDir()
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeFile := func(root, name, data string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string]string{
		sdk.ManifestFile: string(manifestJSON),
		manifest.Entry:   "catalog inspection must not start this worker",
		"package.json":   `{"description":"Fixture description","magpie":{"name":"\u202eConfusing title","icon":"https://example.invalid/icon.svg"}}`,
		"ui/package.json": `{"magpie":{"name":"Fixture suite","icon":"opencode",
			"panels":[{"slot":"sessions","title":"Fixture history","file":"index.html"}]}}`,
		"ui/index.html": "<p>Fixture history</p>",
	} {
		writeFile(source, name, data)
	}
	catalog := extension.NewCatalog(host.Namespace())
	entry, err := catalog.Install(t.Context(), source, func(context.Context, extension.Installed) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	legacy := t.TempDir()
	writeFile(legacy, "package.json", `{"magpie":{"name":"Legacy fixture"}}`)
	legacyJSON, err := json.Marshal(map[string]any{"plugins": []map[string]string{{"spec": legacy}}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(settings.Dir(), "plugins.json", string(legacyJSON))
	mux := http.NewServeMux()
	providerRoutes(mux, nil)

	type listingRow struct {
		Spec, OwnName, Icon string
		Off                 bool
		Serves              []string
		Panels              []pluginPanelInfoJSON
		SDK                 *struct {
			ProviderIDs []string
			Agents      []string
			Description string
			Settings    json.RawMessage
		}
	}
	read := func() listingRow {
		t.Helper()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/plugins", nil).WithContext(t.Context()))
		var state struct{ Plugins []listingRow }
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &state) != nil {
			t.Fatalf("GET /api/plugins: HTTP %d", response.Code)
		}
		if len(state.Plugins) != 2 || state.Plugins[0].Spec != legacy || state.Plugins[0].OwnName != "Legacy fixture" || state.Plugins[0].SDK != nil {
			t.Fatalf("legacy listing changed: %+v", state.Plugins)
		}
		row := state.Plugins[1]
		if row.Spec != "sdk:"+manifest.ID || row.SDK == nil || host.Running(manifest.ID) {
			t.Fatalf("listing omitted or started the SDK module: %+v", row)
		}
		return row
	}

	row := read()
	if row.OwnName != "Fixture suite" || row.Icon != "opencode" || !slices.Equal(row.Serves, []string{"Example Go", "Example Zen"}) {
		t.Errorf("SDK presentation: name=%q, icon=%q, serves=%v", row.OwnName, row.Icon, row.Serves)
	}
	if !slices.Equal(row.Panels, []pluginPanelInfoJSON{{Slot: "sessions", Title: "Fixture history"}}) {
		t.Errorf("SDK panels: %+v", row.Panels)
	}
	if row.SDK.Description != "Fixture description" || len(row.SDK.Settings) == 0 || len(row.SDK.Agents) != 1 {
		t.Errorf("SDK details or options absent: %+v", row.SDK)
	}
	signIns := readProviderSignInState(t, mux).SDKSubscriptions
	wantIDs := []string{provider.ExtensionProviderID(manifest.ID, "primary"), provider.ExtensionProviderID(manifest.ID, "secondary")}
	if !slices.Equal(row.SDK.ProviderIDs, wantIDs) || len(signIns) != len(wantIDs) {
		t.Fatalf("SDK providers in Installed = %v, sign-in grants = %+v", row.SDK.ProviderIDs, signIns)
	}
	for index, item := range signIns {
		if item.ID != wantIDs[index] || item.Name != manifest.Providers[index].Name || item.Icon != "opencode" {
			t.Errorf("SDK subscription did not keep its provider name and package icon: %+v", item)
		}
	}

	if err := host.SetEnabled(t.Context(), manifest.ID, false); err != nil {
		t.Fatal(err)
	}
	row = read()
	if !row.Off || row.OwnName != "Fixture suite" || row.Icon != "opencode" || len(row.Panels) != 1 || len(row.SDK.Settings) != 0 {
		t.Errorf("disabled SDK listing: %+v", row)
	}
	writeFile(filepath.Join(host.Namespace(), filepath.FromSlash(entry.Directory)), "ui/package.json", `{"magpie":{"name":"Unfrozen label","icon":"openai","panels":[{"slot":"sessions","file":"index.html"}]}}`)
	row = read()
	if row.OwnName != manifest.ID || row.Icon != "" || len(row.Panels) != 0 {
		t.Errorf("unchecked presentation reached the listing: %+v", row)
	}
}
