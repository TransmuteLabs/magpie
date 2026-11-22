package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func installWindowDiscoveryFixtures(t *testing.T, catalog *extension.Catalog, count int) extension.Installed {
	t.Helper()
	source := t.TempDir()
	for name, body := range map[string]string{
		"worker":       "fixture must never execute during discovery",
		"package.json": `{"magpie":{"page":{"title":"Fixture","file":"index.html"}}}`,
		"index.html":   "<!doctype html><p>Fixture</p>",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var entry extension.Installed
	for index := range count {
		manifest := sdk.Manifest{Schema: 1, ID: "fixture.window-" + strconv.Itoa(index), Version: "1.0.0", Runtime: "exec", Entry: "worker",
			Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 1}, Capabilities: []sdk.Capability{sdk.ModuleRPC}},
			Window:   &sdk.WindowDescriptor{Metadata: "package.json"}}
		body, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), body, 0o600); err != nil {
			t.Fatal(err)
		}
		entry, err = catalog.Install(t.Context(), source, func(context.Context, extension.Installed) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	return entry
}

func checkWindowDiscoveryPages(t *testing.T, handler http.Handler, count int) {
	t.Helper()
	w := do(handler, httptest.NewRequest(http.MethodGet, "/api/plugin-pages", nil))
	var pages []pluginPage
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &pages) != nil {
		t.Fatalf("window discovery returned status %d: %s", w.Code, w.Body)
	}
	discovered := 0
	for _, page := range pages {
		if strings.HasPrefix(page.Spec, "sdk:fixture.window-") {
			discovered++
		}
	}
	if discovered != count {
		t.Fatalf("window discovery returned %d SDK pages, want %d: %s", discovered, count, w.Body)
	}
}

type windowDiscoveryHost struct {
	*windowTestHost
	fileReads int
}

func (host *windowDiscoveryHost) ReadWindowFile(ctx context.Context, id, revision, filename string) ([]byte, error) {
	host.fileReads++
	return host.Manager.ReadWindowFile(ctx, id, revision, filename)
}

func TestSDKWindowDiscoveryDoesNotReselectEachWindow(t *testing.T) {
	for _, count := range []int{1, 10, 100} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			pagePlugins(t)
			host, catalog := windowTestManager(t)
			installWindowDiscoveryFixtures(t, catalog, count)
			counting := &windowDiscoveryHost{windowTestHost: host}
			extensionWindows = func() extensionWindowHost { return counting }
			checkWindowDiscoveryPages(t, pageMux(), count)
			t.Logf("%d installed windows: %d per-file selections", count, counting.fileReads)
			if counting.fileReads != 0 {
				t.Errorf("window discovery reselected the catalog for %d metadata files", counting.fileReads)
			}
		})
	}
}

func TestSDKWindowChangePollDoesNotReadFrozenMetadata(t *testing.T) {
	pagePlugins(t)
	_, catalog := windowTestManager(t)
	entry := installWindowDiscoveryFixtures(t, catalog, 1)
	metadata := filepath.Join(catalog.Root, filepath.FromSlash(entry.Directory), "package.json")
	if err := os.Chmod(metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, []byte(`{"magpie":{"page":{"title":"Changed","file":"index.html"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mux := pageMux()
	if w := do(mux, httptest.NewRequest(http.MethodGet, "/api/plugin-pages/changes", nil)); w.Code != http.StatusOK {
		t.Fatalf("config-only change poll read window metadata: %d %s", w.Code, w.Body)
	}
	if w := do(mux, httptest.NewRequest(http.MethodGet, "/api/plugin-pages", nil)); w.Code < 400 ||
		!strings.Contains(w.Body.String(), "failed its frozen hash check") {
		t.Fatalf("page discovery did not reject the frozen hash: %d %s", w.Code, w.Body)
	}
}

type windowConfigDiscoveryHost struct {
	*windowTestHost
	afterDiscovery func()
}

func (host *windowConfigDiscoveryHost) WindowsWithMetadata(ctx context.Context) ([]extension.WindowModule, error) {
	modules, err := host.Manager.WindowsWithMetadata(ctx)
	if err == nil && host.afterDiscovery != nil {
		after := host.afterDiscovery
		host.afterDiscovery = nil
		after()
	}
	return modules, err
}

func TestSDKWindowConfigReadRejectsConfigurationChangedAfterDiscovery(t *testing.T) {
	pagePlugins(t)
	host, catalog := windowTestManager(t)
	entry := windowTestInstall(t, catalog, "1.0.0")
	pages, _, err := extensionWindowParts(t.Context())
	if err != nil || len(pages) != 1 {
		t.Fatalf("initial SDK window discovery: %v, %v", pages, err)
	}
	old := pages[0].Ref
	peer := extension.NewManager(catalog.Root)
	t.Cleanup(func() {
		if err := peer.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	current := &windowConfigDiscoveryHost{windowTestHost: host, afterDiscovery: func() {
		if _, err := peer.CompareAndSwapModuleConfig(t.Context(), entry.Manifest.ID, "0", json.RawMessage(`{"label":"updated"}`)); err != nil {
			t.Fatal(err)
		}
	}}
	extensionWindows = func() extensionWindowHost { return current }
	mux := pageMux()
	readConfig := func(ref string) *httptest.ResponseRecorder {
		input := strings.NewReader(`{"ref":"` + ref + `"}`)
		return do(mux, httptest.NewRequest(http.MethodPost, "/api/plugin-pages/config", input))
	}
	if response := readConfig(old); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "module window generation changed") {
		t.Fatalf("configuration read published a retired reference: %d %s", response.Code, response.Body)
	}
	pages, _, err = extensionWindowParts(t.Context())
	if err != nil || len(pages) != 1 || pages[0].Ref == old {
		t.Fatalf("configuration change did not issue a new window reference: %v, %v", pages, err)
	}
	response := readConfig(pages[0].Ref)
	var config sdk.ConfigSnapshot
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &config) != nil ||
		config.Version != "1" || string(config.Values) != `{"label":"updated"}` {
		t.Fatalf("configuration read at the new reference: %d %s", response.Code, response.Body)
	}
}
