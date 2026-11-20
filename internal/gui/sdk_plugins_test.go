package gui

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

const sdkGUIFixture = "gui-opencode"

func sdkGUIManifest(version string) sdk.Manifest {
	manifest := sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: sdkGUIFixture, Version: version, Runtime: "exec", Entry: "worker",
		Contract:      sdk.Contract{Version: sdk.FullContract().Version, Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.AgentWiring, sdk.ModuleRPC}},
		Providers:     []sdk.ProviderDescriptor{{ID: "gui-opencode-go", Name: "Fixture OpenCode Go", Protocols: []protocolext.Protocol{"chat"}}},
		Agents:        []sdk.AgentDescriptor{{ID: "gui-opencode-agent", Name: "Fixture OpenCode Agent"}},
		Compatibility: &sdk.Compatibility{HostAPI: "^1", Platforms: []string{runtime.GOOS + "/" + runtime.GOARCH}},
		Actions:       []sdk.ActionDescriptor{{ID: "config-fixture", InputSchema: json.RawMessage(`{"type":"object"}`), Effect: "read"}},
		RoutingStrategies: []sdk.RoutingStrategyDescriptor{{
			ID: "reverse", Name: "Reverse", Action: "config-fixture", TimeoutMs: 100,
		}},
		Settings: []sdk.FeatureSetting{
			{Name: "recordUsage", Title: "Record usage", Kind: sdk.FeatureSettingBoolean, Default: true},
			{Name: "mode", Title: "Mode", Kind: sdk.FeatureSettingString, Default: "balanced", Enum: []any{"balanced", "fast"}},
			{Name: "attempts", Title: "Attempts", Kind: sdk.FeatureSettingInteger, Default: 2, Minimum: new(float64(1)), Maximum: new(float64(4))},
		},
	}
	if version == "1.3.0" {
		manifest.Settings[1].Enum = []any{"balanced", "slow"}
	}
	return manifest
}

func TestSDKGUIFixtureWorker(t *testing.T) {
	if len(os.Args) != 4 || os.Args[2] != "--" {
		return
	}
	manifest := sdkGUIManifest(os.Args[3])
	handlers := map[string]sdk.Handler{}
	for _, method := range sdk.RequiredMethods(manifest) {
		if method == sdk.MethodModuleCall {
			continue
		}
		handlers[method] = func(context.Context, sdk.Call, *sdk.Stream) (any, error) { return struct{}{}, nil }
	}
	worker := sdk.Module{
		Manifest: manifest, Handlers: handlers,
		Start: func(_ context.Context, configuration sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
			return sdk.ModuleReady{State: configuration.Config}, nil, nil
		},
		Actions: map[string]sdk.Handler{
			"config-fixture": func(ctx context.Context, _ sdk.Call, _ *sdk.Stream) (any, error) {
				current, found := sdk.ModuleContextFrom(ctx)
				start, valid := current.State.(sdk.ConfigSnapshot)
				if !found || !valid || start.Version != current.Config.Version || !bytes.Equal(start.Values, current.Config.Values) {
					return nil, fmt.Errorf("startup and handler observed different configuration snapshots")
				}
				return current.Config, nil
			},
		},
	}
	if err := sdk.Serve(t.Context(), os.Stdin, os.Stdout, worker); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

type sdkGUIReleaseTransport func(*http.Request) (*http.Response, error)

func (f sdkGUIReleaseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func sdkGUIRelease(t *testing.T, version string) []byte {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(sdkGUIManifest(version))
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	files := map[string][]byte{
		sdk.ManifestFile: manifest,
		"package.json":   []byte(`{"name":"gui-opencode","description":"Local release fixture","private":true}`),
		"worker":         []byte("#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestSDKGUIFixtureWorker$ -- " + version + "\n"),
	}
	for name, data := range files {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0o755)
		writer, err := archive.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestOrdinaryPluginRoutesManageSDKRelease(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture worker uses a POSIX launcher")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("MAGPIE_PLUGIN_MARKET", "off")
	t.Setenv("PATH", t.TempDir())
	host := extension.Default()
	oldClient := host.ReleaseClient
	oldAdd, oldUpdate := addPlugin, updatePlugins
	t.Cleanup(func() {
		host.ReleaseClient = oldClient
		addPlugin, updatePlugins = oldAdd, oldUpdate
		if err := host.Close(context.Background()); err != nil {
			t.Error(err)
		}
		sdkPluginChecks.Lock()
		sdkPluginChecks.byID = nil
		sdkPluginChecks.Unlock()
	})
	var legacyAdd string
	var legacyUpdates int
	addPlugin = func(_ context.Context, spec string) (plugin.Entry, error) {
		legacyAdd = spec
		return plugin.Entry{}, nil
	}
	updatePlugins = func(context.Context) error { legacyUpdates++; return nil }

	version := "1.0.0"
	archive := sdkGUIRelease(t, version)
	archiveName := "magpie-plugin_" + sdkGUIFixture + "__" + runtime.GOOS + "_" + runtime.GOARCH + ".zip"
	const downloads = "https://downloads.invalid/"
	var metadataRequests, assetRequests int
	host.ReleaseClient = &http.Client{Transport: sdkGUIReleaseTransport(func(request *http.Request) (*http.Response, error) {
		var data []byte
		switch request.URL.String() {
		case "https://api.github.com/repos/example/module-set/releases?per_page=100&page=1":
			metadataRequests++
			data = fmt.Appendf(nil, `[{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}]`,
				sdkGUIFixture+"-v"+version, archiveName, downloads+archiveName, downloads+"checksums.txt")
		case downloads + archiveName:
			assetRequests++
			data = archive
		case downloads + "checksums.txt":
			assetRequests++
			data = fmt.Appendf(nil, "%x  %s\n", sha256.Sum256(archive), archiveName)
		default:
			return nil, fmt.Errorf("unexpected release request %s", request.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)),
			ContentLength: int64(len(data)), Request: request,
		}, nil
	})}
	mux := http.NewServeMux()
	pluginRoutes(mux, nil)
	call := func(method, tail, body string, target any) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		request := httptest.NewRequest(method, "/api/plugins"+tail, strings.NewReader(body)).WithContext(t.Context())
		mux.ServeHTTP(rec, request)
		if target != nil {
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s: %d %s", method, tail, rec.Code, rec.Body)
			}
			reflect.ValueOf(target).Elem().SetZero()
			if err := json.Unmarshal(rec.Body.Bytes(), target); err != nil {
				t.Fatal(err)
			}
		}
		return rec
	}
	const source = "github:example/module-set#" + sdkGUIFixture
	const idJSON = `{"spec":"sdk:` + sdkGUIFixture + `"}`
	var state pluginsJSON
	if response := call("POST", "/add", `{"spec":"`+source+`"}`, &state); !strings.Contains(response.Body.String(), "Local release fixture") {
		t.Fatalf("checked description is absent: %s", response.Body)
	}
	if legacyAdd != "" || len(state.Plugins) != 1 {
		t.Fatalf("SDK install reached legacy or omitted installed SDK row: %q %+v", legacyAdd, state.Plugins)
	}
	previousServer := served.Swap(nil)
	t.Cleanup(func() { served.Store(previousServer) })
	gui := Handler(nil, nil)
	if response := postBuiltin(gui, `{"id":"middleware","on":false}`); response.Code != http.StatusOK {
		t.Fatalf("turn off JS gateway middleware: %d %s", response.Code, response.Body)
	}
	type routingOption struct{ ID, Name, Plugin string }
	want := routingOption{sdkGUIFixture + "/reverse", "Reverse", sdkGUIFixture}
	checkRoutingOptions := func(active bool) {
		t.Helper()
		for _, endpoint := range []string{"/api/state", "/api/state?last=1", "/api/settings", "/api/providers", "/api/groups", "/api/caller-keys"} {
			response := httptest.NewRecorder()
			gui.ServeHTTP(response, httptest.NewRequest("GET", endpoint, nil).WithContext(t.Context()))
			var data struct{ RoutingOptions, JSRoutings []routingOption }
			if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil || response.Code != http.StatusOK {
				t.Fatalf("%s: %v HTTP %d %s", endpoint, err, response.Code, response.Body)
			}
			if len(data.RoutingOptions) == 0 || data.RoutingOptions[0].ID != "" || slices.Contains(data.RoutingOptions, want) != active {
				t.Fatalf("%s: active = %v, options = %+v", endpoint, active, data.RoutingOptions)
			}
			if endpoint == "/api/settings" && (data.JSRoutings == nil || slices.Contains(data.JSRoutings, want) != active) {
				t.Fatalf("%s: active = %v, legacy projection = %+v", endpoint, active, data.JSRoutings)
			}
		}
		if host.Running(sdkGUIFixture) {
			t.Fatal("listing routing choices started the SDK module")
		}
	}
	checkRoutingOptions(true)
	call("POST", "/off", `{"spec":"sdk:`+sdkGUIFixture+`","off":true}`, &state)
	checkRoutingOptions(false)
	call("POST", "/off", idJSON, &state)
	checkRoutingOptions(true)
	row := state.Plugins[0]
	if row.SDK == nil || row.SDK.Source != source || row.Spec != "sdk:"+sdkGUIFixture || row.Version != version ||
		row.Error != "" || !row.SDK.Checkable || len(row.Providers) != 1 || len(row.SDK.Agents) != 1 ||
		row.SDK.HostAPI != "^1" || len(row.SDK.Protocols) != 1 || len(row.SDK.Capabilities) != 3 {
		t.Fatalf("incorrect installed SDK metadata: %+v %+v", row, row.SDK)
	}
	checkWorkerConfig := func(version, values string) {
		t.Helper()
		body, err := host.Call(t.Context(), sdkGUIFixture, sdk.MethodModuleCall, sdk.Scope{},
			sdk.ModuleCallRequest{Action: "config-fixture", Input: json.RawMessage(`{}`)}, nil)
		var config sdk.ConfigSnapshot
		if err != nil || json.Unmarshal(body, &config) != nil || config.Version != version || string(config.Values) != values {
			t.Fatalf("worker did not receive effective configuration %s %s: %s %v", version, values, body, err)
		}
	}
	checkWorkerConfig("0", `{"attempts":2,"mode":"balanced","recordUsage":true}`)
	raw, err := host.ModuleConfig(t.Context(), sdkGUIFixture)
	if err != nil || raw.Version != "0" || string(raw.Values) != "{}" {
		t.Fatalf("defaults were written to disk: %+v %v", raw, err)
	}
	if _, err := host.CompareAndSwapModuleConfig(t.Context(), sdkGUIFixture, "0", json.RawMessage(`{"private":{"token":"kept-secret"}}`)); err != nil {
		t.Fatal(err)
	}
	shown := call("GET", "", "", &state)
	form := state.Plugins[0].SDK.Settings
	if form == nil || form.ConfigVersion != "1" || form.PackageRevision == "" ||
		!reflect.DeepEqual(form.Schema.Order, []string{"recordUsage", "mode", "attempts"}) ||
		string(form.Values["mode"]) != `"balanced"` || strings.Contains(shown.Body.String(), "kept-secret") {
		t.Fatalf("ordinary listing has wrong settings or leaked unrelated config: %s", shown.Body)
	}
	change := func(revision extension.ModuleSettingsRevision, values string) string {
		t.Helper()
		raw, err := json.Marshal(struct {
			extension.ModuleSettingsRevision
			Spec   string          `json:"spec"`
			Values json.RawMessage `json:"values"`
		}{revision, "sdk:" + sdkGUIFixture, json.RawMessage(values)})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	for _, values := range []string{`{"private":false}`, `{"recordUsage":null}`, `{"attempts":5}`, `{"attempts":2,"attempts":3}`, `{"mode":"unknown"}`} {
		if got := call("POST", "/settings", change(form.ModuleSettingsRevision, values), nil); got.Code == http.StatusOK {
			t.Fatalf("settings accepted invalid patch %s: %s", values, got.Body)
		}
	}
	call("POST", "/settings", change(form.ModuleSettingsRevision, `{"mode":"fast"}`), new(struct{}))
	if got := call("POST", "/settings", change(form.ModuleSettingsRevision, `{"mode":"balanced"}`), nil); got.Code == http.StatusOK {
		t.Fatal("stale config revision rewrote saved settings")
	}
	if _, err := host.CompareAndSwapModuleConfig(t.Context(), sdkGUIFixture, "2", json.RawMessage(`{"attempts":"three"}`)); err == nil {
		t.Fatal("raw module configuration writer bypassed declared type checking")
	}
	checkWorkerConfig("2", `{"attempts":2,"mode":"fast","private":{"token":"kept-secret"},"recordUsage":true}`)
	call("GET", "", "", &state)
	form = state.Plugins[0].SDK.Settings
	if assetRequests != 2 {
		t.Fatalf("installation did not fetch the archive and checksum once: %d", assetRequests)
	}
	version = "1.1.0"
	archive = sdkGUIRelease(t, version)
	before := metadataRequests
	call("GET", "", "", &state)
	var updates plugin.Updates
	call("GET", "/updates", "", &updates)
	if len(updates.Waiting) != 0 || metadataRequests != before {
		t.Fatal("drawing Installed or its update badge queried GitHub")
	}
	var checked struct {
		Plugins []plugin.VersionCheck
		State   pluginsJSON
	}
	call("POST", "/check", "{}", &checked)
	if len(checked.Plugins) != 1 || checked.Plugins[0].Status != "update" ||
		checked.State.Plugins[0].Latest != version || metadataRequests != before+1 || assetRequests != 2 {
		t.Fatalf("Check did not use release metadata only: %+v; metadata %d, assets %d", checked, metadataRequests, assetRequests)
	}
	call("GET", "/updates", "", &updates)
	if len(updates.Waiting) != 1 || updates.Waiting[0].Latest != version {
		t.Fatalf("SDK update missing from ordinary badge: %+v", updates)
	}
	call("POST", "/off", `{"spec":"sdk:`+sdkGUIFixture+`","off":true}`, &state)
	if state.Plugins[0].SDK.Settings != nil {
		t.Fatal("disabled module exposes an active settings form")
	}
	if got := call("POST", "/settings", change(form.ModuleSettingsRevision, `{"mode":"balanced"}`), nil); got.Code == http.StatusOK {
		t.Fatal("disabled module accepted a settings patch")
	}
	call("POST", "/off", idJSON, &state)
	if got := call("POST", "/settings", change(form.ModuleSettingsRevision, `{"mode":"balanced"}`), nil); got.Code == http.StatusOK {
		t.Fatal("old settings form survived disable and re-enable")
	}
	call("POST", "/off", `{"spec":"sdk:`+sdkGUIFixture+`","off":true}`, &state)
	call("GET", "/updates", "", &updates)
	if !state.Plugins[0].Off || state.Plugins[0].Latest != version || len(updates.Waiting) != 0 {
		t.Fatalf("Off did not hide SDK update badge while preserving row: %+v %+v", state, updates)
	}
	call("POST", "/check", "{}", &checked)
	if len(checked.Plugins) != 1 || !checked.Plugins[0].Off || checked.Plugins[0].Status != "update" {
		t.Fatalf("manual check skipped disabled SDK plugin: %+v", checked.Plugins)
	}
	call("POST", "/upgrade", idJSON, &state)
	if state.Plugins[0].Version != version || !state.Plugins[0].Off || state.Plugins[0].Latest != "" || state.Plugins[0].SDK.Check != nil {
		t.Fatalf("Upgrade did not replace revision, preserve Off and clear prior check: %+v", state.Plugins[0])
	}
	call("POST", "/off", idJSON, &state)
	if state.Plugins[0].Off {
		t.Fatal("ordinary on switch left SDK plugin disabled")
	}
	stalePackage := state.Plugins[0].SDK.Settings.ModuleSettingsRevision
	stalePackage.PackageRevision = form.PackageRevision
	if got := call("POST", "/settings", change(stalePackage, `{"mode":"balanced"}`), nil); got.Code == http.StatusOK {
		t.Fatal("configuration form from an old package survived an update")
	}
	checkWorkerConfig("2", `{"attempts":2,"mode":"fast","private":{"token":"kept-secret"},"recordUsage":true}`)
	version = "1.2.0"
	archive = sdkGUIRelease(t, version)
	call("POST", "/update", "{}", &state)
	if state.Plugins[0].Version != version || legacyUpdates != 1 {
		t.Fatalf("Update all did not go through both paths: %+v, %d", state.Plugins[0], legacyUpdates)
	}
	version = "1.3.0"
	archive = sdkGUIRelease(t, version)
	if got := call("POST", "/upgrade", idJSON, nil); got.Code == http.StatusOK || !strings.Contains(got.Body.String(), `module setting \"mode\"`) {
		t.Fatalf("incompatible configuration update was not refused: %d %s", got.Code, got.Body)
	}
	call("GET", "", "", &state)
	if state.Plugins[0].Version != "1.2.0" {
		t.Fatalf("incompatible update replaced the current package: %+v", state.Plugins[0])
	}
	checkWorkerConfig("2", `{"attempts":2,"mode":"fast","private":{"token":"kept-secret"},"recordUsage":true}`)
	call("POST", "/remove", idJSON, &state)
	if len(state.Plugins) != 0 {
		t.Fatalf("ordinary remove left the SDK row: %+v", state.Plugins)
	}
	checkRoutingOptions(false)

	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, sdk.ManifestFile), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(map[string]string{"spec": folder})
	if err != nil {
		t.Fatal(err)
	}
	if bad := call("POST", "/add", string(request), nil); bad.Code == http.StatusOK || legacyAdd != "" {
		t.Fatalf("malformed SDK descriptor fell through to legacy: %d %q", bad.Code, legacyAdd)
	}
	if err := os.Remove(filepath.Join(folder, sdk.ManifestFile)); err != nil {
		t.Fatal(err)
	}
	call("POST", "/add", string(request), &state)
	if legacyAdd != folder {
		t.Fatalf("descriptorless folder didn't enter legacy installer: %q", legacyAdd)
	}
}
