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
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

const sdkGUIFixture = "gui-opencode"

func sdkGUIManifest(version string) sdk.Manifest {
	return sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: sdkGUIFixture, Version: version, Runtime: "exec", Entry: "worker",
		Contract:      sdk.Contract{Version: sdk.FullContract().Version, Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.AgentWiring}},
		Providers:     []sdk.ProviderDescriptor{{ID: "gui-opencode-go", Name: "Fixture OpenCode Go", Protocols: []protocolext.Protocol{"chat"}}},
		Agents:        []sdk.AgentDescriptor{{ID: "gui-opencode-agent", Name: "Fixture OpenCode Agent"}},
		Compatibility: &sdk.Compatibility{HostAPI: "^1", Platforms: []string{runtime.GOOS + "/" + runtime.GOARCH}},
	}
}

func TestSDKGUIFixtureWorker(t *testing.T) {
	if len(os.Args) != 4 || os.Args[2] != "--" {
		return
	}
	manifest := sdkGUIManifest(os.Args[3])
	handlers := map[string]sdk.Handler{}
	for _, method := range sdk.RequiredMethods(manifest) {
		handlers[method] = func(context.Context, sdk.Call, *sdk.Stream) (any, error) { return struct{}{}, nil }
	}
	if err := sdk.Serve(t.Context(), os.Stdin, os.Stdout, sdk.Module{Manifest: manifest, Handlers: handlers}); err != nil {
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
	row := state.Plugins[0]
	if row.SDK == nil || row.SDK.Source != source || row.Spec != "sdk:"+sdkGUIFixture || row.Version != version ||
		row.Error != "" || !row.SDK.Checkable || len(row.Providers) != 1 || len(row.SDK.Agents) != 1 ||
		row.SDK.HostAPI != "^1" || len(row.SDK.Protocols) != 1 || len(row.SDK.Capabilities) != 2 {
		t.Fatalf("incorrect installed SDK metadata: %+v %+v", row, row.SDK)
	}
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
	version = "1.2.0"
	archive = sdkGUIRelease(t, version)
	call("POST", "/update", "{}", &state)
	if state.Plugins[0].Version != version || legacyUpdates != 1 {
		t.Fatalf("Update all did not go through both paths: %+v, %d", state.Plugins[0], legacyUpdates)
	}
	call("POST", "/remove", idJSON, &state)
	if len(state.Plugins) != 0 {
		t.Fatalf("ordinary remove left the SDK row: %+v", state.Plugins)
	}

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
