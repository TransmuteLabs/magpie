package extension

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func moduleReleaseName(id string) string {
	return "magpie-plugin_" + id + "__" + runtime.GOOS + "_" + runtime.GOARCH + ".zip"
}

type moduleReleaseServer struct {
	mu       sync.Mutex
	tag      string
	assets   []releaseAsset
	releases []githubRelease
	payloads map[string][]byte
	requests []releaseAuthRequest
	private  bool
	redirect string
	hold     chan struct{}
	arrived  chan struct{}
	client   *http.Client
	override func(*http.Request) (int, []byte, bool)
}

func newModuleReleaseServer(t *testing.T) *moduleReleaseServer {
	t.Helper()
	fixture := &moduleReleaseServer{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fixture.mu.Lock()
		fixture.requests = append(fixture.requests, releaseAuthRequest{
			location: "https://" + request.Host + request.URL.RequestURI(),
			auth:     request.Header.Get("Authorization"), accept: request.Header.Get("Accept"),
		})
		private, redirect := fixture.private, fixture.redirect
		tag, assets := fixture.tag, append([]releaseAsset(nil), fixture.assets...)
		releases := append([]githubRelease(nil), fixture.releases...)
		override := fixture.override
		payload, found := fixture.payloads[request.URL.Path]
		hold, arrived := fixture.hold, fixture.arrived
		fixture.mu.Unlock()
		if request.Header.Get("Cookie") != "" || request.Header.Get("Proxy-Authorization") != "" {
			t.Error("release request carried ambient credentials")
		}
		if request.Host == "api.github.com" && strings.HasPrefix(request.URL.Path, "/repos/owner/mono/releases") {
			if private && request.Header.Get("Authorization") != "Bearer "+releaseAuthTestToken {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			if override != nil {
				if status, body, handled := override(request); handled {
					writer.WriteHeader(status)
					_, _ = writer.Write(body)
					return
				}
			}
			if strings.Contains(request.URL.Path, "/assets/") {
				if hold != nil {
					select {
					case arrived <- struct{}{}:
					default:
					}
					select {
					case <-hold:
					case <-request.Context().Done():
						return
					}
				}
				if redirect != "" {
					http.Redirect(writer, request, redirect+request.URL.Path, http.StatusFound)
					return
				}
			} else if request.URL.Path == "/repos/owner/mono/releases" {
				_ = json.NewEncoder(writer).Encode(releases)
				return
			} else if request.URL.Path == "/repos/owner/mono/releases/latest" ||
				request.URL.Path == "/repos/owner/mono/releases/tags/"+tag {
				_ = json.NewEncoder(writer).Encode(githubRelease{Tag: tag, Assets: assets})
				return
			} else {
				for _, release := range releases {
					if request.URL.Path == "/repos/owner/mono/releases/tags/"+release.Tag {
						_ = json.NewEncoder(writer).Encode(release)
						return
					}
				}
			}
		} else if request.Header.Get("Authorization") != "" {
			t.Error("credential followed a release redirect")
		}
		if !found {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write(payload)
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := server.Client().Transport
	t.Cleanup(func() { server.Client().CloseIdleConnections() })
	fixture.client = &http.Client{Transport: releaseHTTPFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "https" || request.URL.Host != "api.github.com" && request.URL.Host != "cdn.example.invalid" {
			return nil, fmt.Errorf("offline fixture refused target %s", request.URL)
		}
		copy := request.Clone(request.Context())
		location := *request.URL
		location.Scheme, location.Host = target.Scheme, target.Host
		copy.URL, copy.Host = &location, request.URL.Host
		return transport.RoundTrip(copy)
	})}
	return fixture
}

func (fixture *moduleReleaseServer) publish(t *testing.T, tag string, archives map[string][]byte) {
	t.Helper()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.tag, fixture.assets = tag, nil
	fixture.releases = nil
	fixture.payloads = map[string][]byte{}
	names := make([]string, 0, len(archives))
	for name := range archives {
		names = append(names, name)
	}
	sort.Strings(names)
	var checksums []byte
	for index, name := range names {
		asset := releaseAsset{ID: int64(index + 100), Name: name, URL: "https://cdn.example.invalid/" + name}
		fixture.assets = append(fixture.assets, asset)
		if name != releasePlatformName() && name != "magpie-plugin_any.zip" {
			id := strings.TrimPrefix(name, "magpie-plugin_")
			id = strings.TrimSuffix(id, "__"+runtime.GOOS+"_"+runtime.GOARCH+".zip")
			id = strings.TrimSuffix(id, "__any.zip")
			archive, err := zip.NewReader(bytes.NewReader(archives[name]), int64(len(archives[name])))
			if err != nil {
				t.Fatal(err)
			}
			input, err := archive.Open(sdk.ManifestFile)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(input)
			_ = input.Close()
			var manifest sdk.Manifest
			if err != nil || json.Unmarshal(body, &manifest) != nil {
				t.Fatal("invalid fixture manifest")
			}
			fixture.releases = append(fixture.releases, githubRelease{
				Tag: id + "-v" + manifest.Version, Assets: []releaseAsset{asset, {ID: 200, Name: "checksums.txt"}},
			})
		}
		fixture.payloads[fmt.Sprintf("/repos/owner/mono/releases/assets/%d", asset.ID)] = archives[name]
		checksums = fmt.Appendf(checksums, "%x  %s\n", sha256.Sum256(archives[name]), name)
	}
	fixture.assets = append(fixture.assets, releaseAsset{ID: 200, Name: "checksums.txt"})
	fixture.payloads["/repos/owner/mono/releases/assets/200"] = checksums
	fixture.payloads["/redirected-checksums"] = checksums
}

func (fixture *moduleReleaseServer) count() int {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return len(fixture.requests)
}

func moduleReleaseZIP(t *testing.T, id, version, runtimeName string) []byte {
	t.Helper()
	manifest := releaseTestManifest(runtimeName, version)
	manifest.ID = id
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return releaseTestZIP(t, []releaseZIPEntry{
		{name: sdk.ManifestFile, body: body},
		{name: "bin/worker", body: []byte("preparation fixture only"), mode: 0o700},
	})
}

func TestModuleReleaseSourceGrammarPreservesLegacy(t *testing.T) {
	for _, repository := range []string{"owner/repo", "Owner-9/repo.name_9", "0/a", "a/b."} {
		for _, tag := range []string{"", "@1.2.3", "@v1.2.3", "@v1.2.3-alpha.1+build.9", "@0.0.0"} {
			reference := "github:" + repository + tag
			endpoint, pin, err := releaseSource(reference)
			expected := "https://api.github.com/repos/" + repository + "/releases/latest"
			if tag != "" {
				expected = "https://api.github.com/repos/" + repository + "/releases/tags/" + url.PathEscape(tag[1:])
			}
			if err != nil || endpoint != expected || pin != strings.TrimPrefix(tag, "@") {
				t.Fatalf("legacy reference changed: %q -> %q %q %v", reference, endpoint, pin, err)
			}
		}
	}
	for _, id := range []string{"a", "a__b", "a_b.c-9", "a__", strings.Repeat("a", 128)} {
		for _, tag := range []string{"", "@" + id + "-v1.2.3-alpha.1+build.9"} {
			reference := "github:owner/repo#" + id + tag
			endpoint, pin, err := releaseSource(reference)
			expected := "https://api.github.com/repos/owner/repo/releases"
			if tag != "" {
				expected = "https://api.github.com/repos/owner/repo/releases/tags/" + url.PathEscape(tag[1:])
			}
			if err != nil || endpoint != expected || pin != strings.TrimPrefix(tag, "@") {
				t.Fatalf("scoped reference changed repository or pin: %q -> %q %q %v", reference, endpoint, pin, err)
			}
		}
	}
	if releaseTag("v1.2.3#text") {
		t.Fatal("legacy semantic version grammar unexpectedly permits a fragment")
	}
	for _, reference := range []string{
		"github:owner/repo@v1.2.3#module", "github:owner/repo@v1.2.3-alpha#module",
		"github:owner/repo#", "github:owner/repo#../module", "github:owner/repo##module",
		"github:owner/repo#module@", "github:owner/repo#module@bad",
		"github:owner/repo#module/other", "github:owner/repo#module%23other",
		"github:owner/repo#Module", "github:owner/repo#module@v1.2.3@v2.0.0",
		"github:owner/repo#module@v1.2.3", "github:owner/repo#module@other-v1.2.3",
	} {
		if _, _, err := releaseSource(reference); err == nil {
			t.Fatalf("ambiguous or invalid source accepted: %q", reference)
		}
	}
}

func TestModuleReleaseScopedUniversalAndPublicationIdentity(t *testing.T) {
	for _, runtimeName := range []string{"exec", "bun"} {
		t.Run(runtimeName, func(t *testing.T) {
			fixture := newModuleReleaseServer(t)
			fixture.publish(t, "v9.0.0", map[string][]byte{
				"magpie-plugin_module__any.zip": moduleReleaseZIP(t, "module", "1.2.3", runtimeName),
				"magpie-plugin_any.zip":         releaseTestPackage(t, "bun", "9.0.0"),
			})
			prepared, cleanup, err := PrepareRelease(t.Context(), fixture.client, "github:owner/mono#module")
			if runtimeName == "exec" {
				if err == nil {
					cleanup()
					t.Fatal("scoped universal archive accepted an executable")
				}
			} else {
				if err != nil {
					t.Fatal("scoped Bun universal fallback failed:", err)
				}
				if prepared.Source.Archive != "magpie-plugin_module__any.zip" {
					t.Fatal("scoped fallback selected another module or legacy archive")
				}
				cleanup()
			}
		})
	}
	catalog := NewCatalog(t.TempDir())
	directory := catalogTestPackage(t, "other", "1.0.0")
	source := PackageSource{
		Reference: "github:owner/mono#wanted", Tag: "wanted-v1.0.0",
		Archive: moduleReleaseName("wanted"), SHA256: strings.Repeat("a", 64),
	}
	checked := false
	if _, err := catalog.install(t.Context(), directory, func(context.Context, Installed) error {
		checked = true
		return nil
	}, installOptions{source: &source}); err == nil || checked {
		t.Fatal("wrong selected identity reached handshake or publication")
	}
	entry := catalogTestInstall(t, catalog, directory)
	entry.Source = &source
	if err := catalogValidate(entry); err == nil {
		t.Fatal("persisted provenance accepted a mismatched selected ID")
	}
}

func TestModuleReleaseUpdateCannotResurrectRemovedModule(t *testing.T) {
	fixture := newModuleReleaseServer(t)
	manager := NewManager(t.TempDir())
	manager.ReleaseClient = fixture.client
	t.Cleanup(func() { managerReleaseClose(t, manager) })
	directory := moduleReleaseWorker(t, "module", "1.0.0")
	archive := managerReleaseArchive(t, directory)
	fixture.publish(t, "v9.0.0", map[string][]byte{moduleReleaseName("module"): archive})
	if _, err := manager.Install(t.Context(), "github:owner/mono#module"); err != nil {
		t.Fatal(err)
	}
	hold, arrived := make(chan struct{}), make(chan struct{}, 1)
	fixture.mu.Lock()
	fixture.hold, fixture.arrived = hold, arrived
	fixture.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() { close(hold) })
		<-done
	})
	go func() {
		_, err := manager.Update(ctx, "module")
		done <- err
		close(done)
	}()
	select {
	case <-arrived:
	case <-ctx.Done():
		t.Fatal("selected update did not reach held download:", ctx.Err())
	}
	if err := manager.Remove(ctx, "module"); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(hold) })
	select {
	case err := <-done:
		if !errors.Is(err, ErrCatalogConflict) {
			t.Fatalf("selected update lost removal precondition: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("selected update outlived its fixture")
	}
	entries, err := manager.List()
	if err != nil || len(entries) != 0 {
		t.Fatalf("selected update resurrected removed module: %+v %v", entries, err)
	}
}

func TestModuleReleasePrepareTwoModulesAndProvenance(t *testing.T) {
	root := releasePrivateTemps(t)
	fixture := newModuleReleaseServer(t)
	fixture.publish(t, "v9.0.0", map[string][]byte{
		moduleReleaseName("module_a__one"): moduleReleaseZIP(t, "module_a__one", "1.2.3", "exec"),
		moduleReleaseName("module-b"):      moduleReleaseZIP(t, "module-b", "4.5.6", "exec"),
		releasePlatformName():              releaseTestPackage(t, "exec", "9.0.0"),
	})
	for _, id := range []string{"module_a__one", "module-b"} {
		version := "1.2.3"
		if id == "module-b" {
			version = "4.5.6"
		}
		for _, pin := range []string{"", "@" + id + "-v" + version} {
			reference := "github:owner/mono#" + id + pin
			prepared, cleanup, err := PrepareRelease(t.Context(), fixture.client, reference)
			if err != nil {
				t.Fatalf("selected module %s could not be prepared: %v", reference, err)
			}
			if prepared.Source.Reference != reference || prepared.Source.Archive != moduleReleaseName(id) ||
				prepared.Source.Tag != id+"-v"+version || prepared.Source.Validate() != nil {
				t.Fatalf("scoped provenance lost selector or tag: %+v", prepared.Source)
			}
			var manifest sdk.Manifest
			body, err := os.ReadFile(filepath.Join(prepared.Directory, sdk.ManifestFile))
			if err != nil || json.Unmarshal(body, &manifest) != nil || manifest.ID != id {
				t.Fatalf("wrong selected manifest: %+v %v", manifest, err)
			}
			historical := prepared.Source
			historical.Archive = "magpie-plugin_" + id + "__windows_amd64.zip"
			if err := historical.Validate(); err != nil {
				t.Fatal("historical scoped provenance rejected:", err)
			}
			historical.Archive = moduleReleaseName("another")
			if historical.Validate() == nil {
				t.Fatal("provenance accepted another module's archive")
			}
			cleanup()
		}
	}
	prepared, cleanup, err := PrepareRelease(t.Context(), fixture.client, "github:owner/mono")
	if err != nil {
		t.Fatal("legacy package selection changed:", err)
	}
	if prepared.Source.Archive != releasePlatformName() {
		t.Fatal("legacy reference selected a module-scoped asset")
	}
	cleanup()
	releaseAssertEmpty(t, root)
}

func TestModuleReleaseRejectsWrongIDAndMissingScopedAsset(t *testing.T) {
	for _, test := range []string{"wrong manifest", "legacy only", "other module only", "duplicate selected", "bad checksum"} {
		t.Run(test, func(t *testing.T) {
			root := releasePrivateTemps(t)
			fixture := newModuleReleaseServer(t)
			name := moduleReleaseName("wanted")
			body := moduleReleaseZIP(t, "wanted", "1.2.3", "exec")
			switch test {
			case "wrong manifest":
				body = moduleReleaseZIP(t, "other", "1.2.3", "exec")
			case "legacy only":
				name = releasePlatformName()
			case "other module only":
				name = moduleReleaseName("other")
			}
			fixture.publish(t, "v1.2.3", map[string][]byte{name: body})
			if test == "duplicate selected" {
				fixture.releases[0].Assets = append(fixture.releases[0].Assets, fixture.releases[0].Assets[0])
			}
			if test == "bad checksum" {
				fixture.payloads["/repos/owner/mono/releases/assets/200"] = []byte(strings.Repeat("a", 64) + "  " + name + "\n")
			}
			prepared, cleanup, err := PrepareRelease(t.Context(), fixture.client, "github:owner/mono#wanted")
			if err == nil || cleanup != nil || prepared.Directory != "" {
				if cleanup != nil {
					cleanup()
				}
				t.Fatal("invalid scoped release escaped preparation")
			}
			releaseAssertEmpty(t, root)
		})
	}
}

func TestModuleReleasePrivateAndRedirectFences(t *testing.T) {
	for _, redirect := range []string{"", "https://cdn.example.invalid", "http://cdn.example.invalid"} {
		t.Run(redirect, func(t *testing.T) {
			fixture := newModuleReleaseServer(t)
			fixture.private, fixture.redirect = true, redirect
			fixture.publish(t, "v1.2.3", map[string][]byte{
				moduleReleaseName("module_a__one"): moduleReleaseZIP(t, "module_a__one", "1.2.3", "exec"),
				moduleReleaseName("module-b"):      moduleReleaseZIP(t, "module-b", "1.2.3", "exec"),
			})
			lookups := 0
			prepared, cleanup, err := prepareRelease(t.Context(), fixture.client, "github:owner/mono#module_a__one@module_a__one-v1.2.3",
				func(_ context.Context, repository string) (string, error) {
					lookups++
					if repository != "owner/mono" {
						t.Fatal("credential lookup included selector:", repository)
					}
					return releaseAuthTestToken, nil
				})
			if strings.HasPrefix(redirect, "http:") {
				if err == nil {
					cleanup()
					t.Fatal("redirect fixture unexpectedly produced a valid archive")
				}
			} else {
				if err != nil {
					t.Fatal("private selected module failed:", err)
				}
				provenance, _ := json.Marshal(prepared.Source)
				if bytes.Contains(provenance, []byte(releaseAuthTestToken)) {
					t.Fatal("private release token persisted in provenance")
				}
				cleanup()
			}
			if lookups != 1 {
				t.Fatalf("private release resolved credentials %d times", lookups)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			for index, request := range fixture.requests {
				api := strings.HasPrefix(request.location, "https://api.github.com/repos/owner/mono/releases")
				if index == 0 && request.auth != "" || index > 0 && api && request.auth != "Bearer "+releaseAuthTestToken ||
					!api && request.auth != "" {
					t.Fatal("private auth or redirect scope changed")
				}
			}
		})
	}
}

func moduleReleaseWorker(t *testing.T, id, version string) string {
	t.Helper()
	directory := fixturePackage(t)
	body, err := os.ReadFile(filepath.Join(directory, sdk.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.ID, manifest.Version = id, version
	manifest.Contract.Capabilities = []sdk.Capability{sdk.ToolProvider, sdk.OperationControl, sdk.ModuleRPC}
	manifest.Inbounds, manifest.Providers, manifest.Agents, manifest.Sessions = nil, nil, nil, nil
	manifest.Tools[0].Name = id + "-tool"
	catalogTestWriteManifest(t, directory, manifest)
	return directory
}

func TestModuleReleaseManagerTracksIndependentVersions(t *testing.T) {
	t.Setenv("GH_TOKEN", releaseAuthTestToken)
	fixture := newModuleReleaseServer(t)
	fixture.private = true
	manager := NewManager(t.TempDir())
	manager.ReleaseClient = fixture.client
	t.Cleanup(func() { managerReleaseClose(t, manager) })
	firstDirectory := moduleReleaseWorker(t, "module_a__one", "1.0.0")
	secondDirectory := moduleReleaseWorker(t, "module-b", "4.0.0")
	const description = "A module from an independently versioned GitHub release"
	catalogTestWrite(t, filepath.Join(firstDirectory, "package.json"), []byte(`{"description":"`+description+`"}`), 0o600)
	firstArchive := managerReleaseArchive(t, firstDirectory)
	secondArchive := managerReleaseArchive(t, secondDirectory)
	fixture.publish(t, "v9.0.0", map[string][]byte{
		moduleReleaseName("module_a__one"): firstArchive, moduleReleaseName("module-b"): secondArchive,
	})
	firstReference, secondReference := "github:owner/mono#module_a__one", "github:owner/mono#module-b"
	if recognized, err := manager.ProbePluginRelease(t.Context(), firstReference); err != nil || !recognized {
		t.Fatalf("private module not recognized through ordinary add: %t %v", recognized, err)
	}
	if recognized, err := manager.ProbePluginRelease(t.Context(), "github:owner/mono#git-branch"); err != nil || recognized {
		t.Fatalf("ordinary Git branch mistaken for a module: %t %v", recognized, err)
	}
	first, err := manager.Install(t.Context(), firstReference)
	if err != nil {
		t.Fatal("real installer rejected first selected module:", err)
	}
	second, err := manager.Install(t.Context(), secondReference)
	if err != nil {
		t.Fatal("real installer rejected second selected module:", err)
	}
	if text := manager.PluginDescription(t.Context(), first); text != description {
		t.Fatalf("installed description = %q", text)
	}
	if text := manager.PluginDescription(t.Context(), second); text != "" {
		t.Fatalf("old distribution claims a description: %q", text)
	}
	foreign := first
	foreign.Files = map[string]string{"package.json": "old-revision"}
	if text := manager.PluginDescription(t.Context(), foreign); text != "" {
		t.Fatalf("unexpected description for a mismatched file hash: %q", text)
	}
	for _, entry := range []Installed{first, second} {
		result, err := manager.Call(t.Context(), entry.Manifest.ID, sdk.MethodToolCall, sdk.Scope{},
			sdk.ToolRequest{ID: entry.Manifest.Tools[0].Name, Arguments: json.RawMessage(`{"action":"pid"}`)}, nil)
		if err != nil || !bytes.Contains(result, []byte(`"pid"`)) {
			t.Fatalf("selected installed worker did not execute: %s %v", result, err)
		}
	}
	private := json.RawMessage(`{"saved":"synthetic-private-state"}`)
	if _, err := manager.Store().StateCAS(sdk.Scope{Plugin: first.Manifest.ID}, "settings", 0, private); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetEnabled(t.Context(), first.Manifest.ID, false); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(firstDirectory, sdk.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Version = "2.0.0"
	catalogTestWriteManifest(t, firstDirectory, manifest)
	newArchive := managerReleaseArchive(t, firstDirectory)
	publish := func(tag string, archive []byte) {
		fixture.publish(t, tag, map[string][]byte{
			moduleReleaseName(first.Manifest.ID): archive, moduleReleaseName(second.Manifest.ID): secondArchive,
		})
	}
	publish("v10.0.0", newArchive)
	beforeCheck := fixture.count()
	available, err := manager.CheckUpdate(t.Context(), first)
	if err != nil || available != (ReleaseAvailability{Version: "2.0.0", Available: true}) {
		t.Fatalf("check got %+v %v", available, err)
	}
	fixture.mu.Lock()
	checked := append([]releaseAuthRequest(nil), fixture.requests[beforeCheck:]...)
	fixture.mu.Unlock()
	for _, request := range checked {
		if strings.Contains(request.location, "/assets/") {
			t.Fatal("checking for updates downloaded an asset:", request.location)
		}
	}
	updated, err := manager.Update(t.Context(), first.Manifest.ID)
	if err != nil || !updated.Disabled || updated.Manifest.Version != "2.0.0" ||
		updated.Directory == first.Directory || updated.Source == nil || updated.Source.Reference != firstReference {
		t.Fatalf("selected update lost identity/version/disable/source: %+v %v", updated, err)
	}
	state, err := manager.Store().StateGet(sdk.Scope{Plugin: first.Manifest.ID}, "settings")
	if err != nil || !bytes.Equal(state.Value, private) {
		t.Fatal("selected update lost private state:", err)
	}
	entries, err := manager.List()
	if err != nil || len(entries) != 2 {
		t.Fatalf("selected update changed sibling membership: %+v %v", entries, err)
	}
	for _, entry := range entries {
		if entry.Manifest.ID == second.Manifest.ID && entry.Revision != second.Revision {
			t.Fatal("selected update changed sibling generation")
		}
	}
	for _, test := range []struct {
		tag     string
		archive []byte
	}{
		{"v11.0.0", newArchive},
		{"v12.0.0", firstArchive},
	} {
		publish(test.tag, test.archive)
		unchanged, err := manager.Update(t.Context(), first.Manifest.ID)
		if err != nil || unchanged.Revision != updated.Revision {
			t.Fatalf("collection tag bypassed unchanged/downgrade fence: %+v %v", unchanged, err)
		}
	}
	catalogTestWrite(t, filepath.Join(firstDirectory, "republished"), []byte("different bytes"), 0o600)
	publish("v13.0.0", managerReleaseArchive(t, firstDirectory))
	if _, err := manager.Update(t.Context(), first.Manifest.ID); err == nil {
		t.Fatal("collection tag allowed changed bytes at same module version")
	}
	publish("v14.0.0", secondArchive)
	if _, err := manager.Update(t.Context(), first.Manifest.ID); err == nil {
		t.Fatal("selected update accepted another module's manifest")
	}
	entries, err = manager.List()
	if err != nil || len(entries) != 2 {
		t.Fatalf("rejected identity substitution changed catalog: %+v %v", entries, err)
	}
	for _, entry := range entries {
		if entry.Manifest.ID == first.Manifest.ID && entry.Revision != updated.Revision {
			t.Fatal("rejected update replaced the existing generation")
		}
	}
	publish("v15.0.0", newArchive)
	pinnedReference := firstReference + "@module_a__one-v2.0.0"
	pinned, err := manager.Install(t.Context(), pinnedReference)
	if err != nil || pinned.Source == nil || pinned.Source.Reference != pinnedReference {
		t.Fatalf("explicit selected pin failed: %+v %v", pinned, err)
	}
	requests := fixture.count()
	if err := manager.Restart(t.Context(), first.Manifest.ID); err != nil {
		t.Fatal("local reload lost selected pin:", err)
	}
	entries, err = manager.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Manifest.ID == first.Manifest.ID {
			pinned = entry
		}
	}
	unchanged, err := manager.Update(t.Context(), first.Manifest.ID)
	if err != nil || unchanged.Revision != pinned.Revision || fixture.count() != requests {
		t.Fatalf("selected pinned update or reload accessed release: %+v %v", unchanged, err)
	}
}
