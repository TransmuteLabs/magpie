package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestInstalledPluginOptionsKeepErrorsAndOwnersPerRow(t *testing.T) {
	host, handler, ownersFile, ids := installedPluginOptionsFixture(t, 3)
	if _, err := host.Store().StateCASContext(t.Context(), sdk.Scope{Plugin: ids[1]}, "module.config", 0,
		json.RawMessage(`{"mode":"invalid"}`)); err != nil {
		t.Fatal(err)
	}
	read := func() []pluginEntryJSON {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/plugins", nil).WithContext(t.Context()))
		var state pluginsJSON
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &state) != nil || state.SDKError != "" {
			t.Fatalf("installed plugins listing: HTTP %d, SDK error %q", response.Code, state.SDKError)
		}
		if len(state.Plugins) != len(ids)+1 {
			t.Fatalf("installed plugin row count: %d", len(state.Plugins))
		}
		for _, id := range ids {
			if host.Running(id) {
				t.Fatalf("listing started %s", id)
			}
		}
		return state.Plugins
	}
	rows := read()
	if !slices.Equal(rows[0].Providers, []string{"Legacy 1", "Legacy 2"}) {
		t.Fatalf("legacy ownership projection: %v", rows[0].Providers)
	}
	first := rows[1]
	if first.Error != "" || first.SDK == nil || first.SDK.Settings == nil ||
		!slices.Equal(first.SDK.ProviderIDs, []string{"owned-0"}) ||
		string(first.SDK.Settings.Values["mode"]) != `"balanced"` {
		t.Fatalf("active owner and options: %+v, %+v", first, first.SDK)
	}
	pending := rows[2]
	if pending.Error == "" || pending.SDK == nil || pending.SDK.Settings != nil || len(pending.SDK.ProviderIDs) != 0 {
		t.Fatalf("pending owner and invalid options: %+v, %+v", pending, pending.SDK)
	}
	inactive := rows[3]
	if inactive.Error != "" || inactive.SDK == nil || inactive.SDK.Settings == nil || len(inactive.SDK.ProviderIDs) != 0 {
		t.Fatalf("inactive owner and options: %+v, %+v", inactive, inactive.SDK)
	}
	if err := os.WriteFile(ownersFile, []byte(`{"truncated":`), 0o600); err != nil {
		t.Fatal(err)
	}
	rows = read()
	for _, row := range rows {
		if len(row.Providers) != 0 || row.SDK != nil && len(row.SDK.ProviderIDs) != 0 {
			t.Fatalf("unreadable ownership exposed providers: %+v", row)
		}
	}
	if rows[1].SDK.Settings == nil || rows[2].Error == "" || rows[3].SDK.Settings == nil {
		t.Fatal("unreadable ownership changed unrelated options")
	}
}

func BenchmarkInstalledPluginOptionsListing(b *testing.B) {
	_, handler, _, ids := installedPluginOptionsFixture(b, 12)
	request := httptest.NewRequest(http.MethodGet, "/api/plugins", nil).WithContext(b.Context())
	check := httptest.NewRecorder()
	handler.ServeHTTP(check, request)
	var state pluginsJSON
	if check.Code != http.StatusOK || json.Unmarshal(check.Body.Bytes(), &state) != nil || len(state.Plugins) != len(ids)+1 {
		b.Fatal("GET /api/plugins did not return the fixture rows")
	}
	for _, row := range state.Plugins[1:] {
		if row.SDK == nil || row.SDK.Settings == nil {
			b.Fatalf("missing plugin options: %s", row.Spec)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			b.Fatalf("GET /api/plugins: HTTP %d", response.Code)
		}
	}
}

func installedPluginOptionsFixture(tb testing.TB, count int) (*extension.Manager, http.Handler, string, []string) {
	tb.Helper()
	home := tb.TempDir()
	tb.Setenv("HOME", home)
	tb.Setenv("USERPROFILE", home)
	tb.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	tb.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	tb.Setenv("APPDATA", filepath.Join(home, "config"))
	tb.Setenv("LOCALAPPDATA", filepath.Join(home, "cache"))
	tb.Setenv("MAGPIE_PLUGIN_MARKET", "off")
	tb.Setenv("MAGPIE_BUN", "")
	tb.Setenv("PATH", "")
	host := extension.Default()
	tb.Cleanup(func() {
		plugin.UseCached(nil)
		if err := host.Close(context.Background()); err != nil {
			tb.Error(err)
		}
	})
	write := func(path string, data []byte, mode os.FileMode) {
		tb.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			tb.Fatal(err)
		}
	}
	legacy := tb.TempDir()
	write(filepath.Join(legacy, "package.json"), []byte(`{"name":"legacy-fixture"}`), 0o600)
	write(filepath.Join(settings.Dir(), "plugins.json"), fmt.Appendf(nil, `{"plugins":[{"spec":%q}]}`, legacy), 0o600)
	catalog := extension.NewCatalog(host.Namespace())
	source := tb.TempDir()
	type ownerRecord struct {
		extension.SubscriptionOwner
		GrantRevision string            `json:"grantRevision"`
		Digests       map[string]string `json:"digests"`
		Tickets       []string          `json:"tickets"`
	}
	var ids []string
	var legacyProviders []plugin.Provider
	var owners []ownerRecord
	for index := range count {
		manifest := sdkGUIManifest("1.0.0")
		manifest.ID = fmt.Sprintf("options-%02d", index)
		id := fmt.Sprintf("listing-%d", index)
		manifest.Providers[0].ID = id
		manifest.Agents[0].ID = id + "-agent"
		if runtime.GOOS == "windows" {
			manifest.Entry += ".exe"
		}
		manifestJSON, err := json.Marshal(manifest)
		if err != nil {
			tb.Fatal(err)
		}
		write(filepath.Join(source, sdk.ManifestFile), manifestJSON, 0o600)
		write(filepath.Join(source, manifest.Entry), []byte("listing must not launch this worker"), 0o700)
		if _, err := catalog.Install(tb.Context(), source, func(context.Context, extension.Installed) error { return nil }); err != nil {
			tb.Fatal(err)
		}
		ids = append(ids, manifest.ID)
		legacyProviders = append(legacyProviders, plugin.Provider{ID: id, Name: fmt.Sprintf("Legacy %d", index), Spec: legacy})
		owners = append(owners, ownerRecord{
			SubscriptionOwner: extension.SubscriptionOwner{
				Module: manifest.ID, Provider: id, PublicID: fmt.Sprintf("owned-%d", index),
				Revision: "owner-revision", Active: index%3 == 0, Pending: index%3 == 1,
				Accounts: []extension.SubscriptionAccount{{Key: id, ID: "fixture", User: "Fixture user", LoginID: "fixture"}},
			},
			GrantRevision: strings.Repeat("a", 64), Digests: map[string]string{id: strings.Repeat("b", 64)}, Tickets: []string{"ticket"},
		})
	}
	ownerJSON, err := json.Marshal(owners)
	if err != nil {
		tb.Fatal(err)
	}
	ownersFile := filepath.Join(host.Namespace(), "private", ".saved-signins", "subscription-owners.json")
	write(ownersFile, ownerJSON, 0o600)
	plugin.UseCached(legacyProviders)
	return host, Handler(nil, nil), ownersFile, ids
}
