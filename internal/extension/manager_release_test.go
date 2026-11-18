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
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type managerReleaseTransport func(*http.Request) (*http.Response, error)

func (roundTrip managerReleaseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func managerReleaseArchive(t *testing.T, directory string) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	err := filepath.WalkDir(directory, func(filename string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if item.IsDir() {
			return nil
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(directory, filename)
		if err != nil {
			return err
		}
		header.Name, header.Method = filepath.ToSlash(name), zip.Deflate
		output, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		input, err := os.Open(filename)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		return errors.Join(copyErr, input.Close())
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func managerReleaseFixtureClient(version string, archive []byte) *http.Client {
	asset := "magpie-plugin_" + runtime.GOOS + "_" + runtime.GOARCH + ".zip"
	metadata, _ := json.Marshal(map[string]any{
		"tag_name": "v" + version,
		"assets": []map[string]string{
			{"name": asset, "browser_download_url": "https://github.com/fixture/module/releases/download/v" + version + "/" + asset},
			{"name": "checksums.txt", "browser_download_url": "https://github.com/fixture/module/releases/download/v" + version + "/checksums.txt"},
		},
	})
	return &http.Client{Transport: managerReleaseTransport(func(request *http.Request) (*http.Response, error) {
		var body []byte
		switch request.URL.String() {
		case "https://api.github.com/repos/fixture/module/releases/tags/v" + version,
			"https://api.github.com/repos/fixture/module/releases/latest":
			body = metadata
		case "https://github.com/fixture/module/releases/download/v" + version + "/" + asset:
			body = archive
		case "https://github.com/fixture/module/releases/download/v" + version + "/checksums.txt":
			body = fmt.Appendf(nil, "%x  %s\n", sha256.Sum256(archive), asset)
		default:
			return nil, fmt.Errorf("unexpected fixture request: %s", request.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: request,
		}, nil
	})}
}

func managerReleaseClose(t *testing.T, manager *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Error(err)
	}
}

func TestManagerInstallsReleaseAndRetainsWorkingGenerationOnFailure(t *testing.T) {
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { managerReleaseClose(t, manager) })
	source := fixturePackage(t)
	manager.ReleaseClient = managerReleaseFixtureClient("1.0.0", managerReleaseArchive(t, source))
	first, err := manager.Install(t.Context(), "github:fixture/module@v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	result := toolCall(t, manager, sdk.Scope{}, "pid", nil)
	if !bytes.Contains(result, []byte(`"pid"`)) {
		t.Fatalf("installed executable did not run: %s", result)
	}
	broken := catalogTestPackage(t, "fixture", "2.0.0")
	manager.ReleaseClient = managerReleaseFixtureClient("2.0.0", managerReleaseArchive(t, broken))
	if _, err := manager.Install(t.Context(), "github:fixture/module@v2.0.0"); err == nil {
		t.Fatal("release with invalid executable passed activation")
	}
	entries, err := manager.List()
	if err != nil || len(entries) != 1 || entries[0].Revision != first.Revision {
		t.Fatalf("failed replacement changed catalog: %+v, %v", entries, err)
	}
	if result := toolCall(t, manager, sdk.Scope{}, "pid", nil); !bytes.Contains(result, []byte(`"pid"`)) {
		t.Fatalf("failed release stopped the previous executable: %s", result)
	}
	if err := manager.catalog.Verify(first); err != nil {
		t.Fatal("failed release damaged prior package:", err)
	}
	if err := manager.SetEnabled(t.Context(), "fixture", false); err != nil {
		t.Fatal(err)
	}
	catalogTestWrite(t, filepath.Join(source, "release-note"), []byte("a distinct immutable generation"), 0o600)
	manager.ReleaseClient = managerReleaseFixtureClient("1.0.0", managerReleaseArchive(t, source))
	replacement, err := manager.Install(t.Context(), "github:fixture/module@v1.0.0")
	if err != nil || !replacement.Disabled || replacement.Directory == first.Directory {
		t.Fatalf("release replacement lost disabled state or generation: %+v, %v", replacement, err)
	}
	if err := manager.catalog.Verify(first); err != nil {
		t.Fatal("successful release removed the previous generation:", err)
	}
}

func TestManagerCloseCancelsReleaseDownload(t *testing.T) {
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { managerReleaseClose(t, manager) })
	started := make(chan struct{})
	var once sync.Once
	manager.ReleaseClient = &http.Client{Transport: managerReleaseTransport(func(request *http.Request) (*http.Response, error) {
		once.Do(func() { close(started) })
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := manager.Install(ctx, "github:fixture/module@v1.0.0")
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("fixture release request did not start:", ctx.Err())
	}
	managerReleaseClose(t, manager)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("release installation survived manager shutdown: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("release download outlived manager shutdown")
	}
	entries, err := manager.List()
	if err != nil || len(entries) != 0 {
		t.Fatalf("interrupted download published a package: %+v, %v", entries, err)
	}
}

func TestManagerReleaseUpdatesRespectVersionSourceAndPrivateState(t *testing.T) {
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { managerReleaseClose(t, manager) })
	source := fixturePackage(t)
	firstArchive := managerReleaseArchive(t, source)
	manager.ReleaseClient = managerReleaseFixtureClient("1.0.0", firstArchive)
	first, err := manager.Install(t.Context(), "github:fixture/module")
	if err != nil {
		t.Fatal(err)
	}
	if first.Source == nil || first.Source.Reference != "github:fixture/module" || first.Source.Tag != "v1.0.0" {
		t.Fatalf("release provenance was not saved: %+v", first.Source)
	}
	private := json.RawMessage(`{"saved":"keep across updates"}`)
	if _, err := manager.Store().StateCAS(sdk.Scope{Plugin: "fixture"}, "settings", 0, private); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetEnabled(t.Context(), "fixture", false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(source, sdk.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Version = "2.0.0"
	catalogTestWriteManifest(t, source, manifest)
	secondArchive := managerReleaseArchive(t, source)
	manager.ReleaseClient = managerReleaseFixtureClient("2.0.0", secondArchive)
	updated, err := manager.Update(t.Context(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Disabled || updated.Manifest.Version != "2.0.0" || updated.Directory == first.Directory ||
		updated.Source == nil || updated.Source.Reference != "github:fixture/module" {
		t.Fatalf("invalid update: %+v", updated)
	}
	state, err := manager.Store().StateGet(sdk.Scope{Plugin: "fixture"}, "settings")
	if err != nil || !bytes.Equal(state.Value, private) {
		t.Fatalf("update lost private state: %+v %v", state, err)
	}
	unchanged, err := manager.Update(t.Context(), "fixture")
	if err != nil || unchanged.Revision != updated.Revision {
		t.Fatalf("unchanged release was reactivated: %+v %v", unchanged, err)
	}
	manager.ReleaseClient = managerReleaseFixtureClient("1.0.0", firstArchive)
	unchanged, err = manager.Update(t.Context(), "fixture")
	if err != nil || unchanged.Revision != updated.Revision {
		t.Fatalf("latest alias downgraded a newer installed version: %+v %v", unchanged, err)
	}
	catalogTestWrite(t, filepath.Join(source, "republished"), []byte("same version, different bytes"), 0o600)
	manager.ReleaseClient = managerReleaseFixtureClient("2.0.0", managerReleaseArchive(t, source))
	if _, err := manager.Update(t.Context(), "fixture"); err == nil {
		t.Fatal("changed bytes silently replaced the same published version")
	}
	entries, err := manager.List()
	if err != nil || len(entries) != 1 || entries[0].Revision != updated.Revision {
		t.Fatalf("refused republish changed installed version: %+v %v", entries, err)
	}
	manifest.ID, manifest.Version = "different-module", "3.0.0"
	catalogTestWriteManifest(t, source, manifest)
	manager.ReleaseClient = managerReleaseFixtureClient("3.0.0", managerReleaseArchive(t, source))
	if _, err := manager.Update(t.Context(), "fixture"); err == nil {
		t.Fatal("update accepted a changed module identity")
	}
	entries, err = manager.List()
	if err != nil || len(entries) != 1 || entries[0].Revision != updated.Revision {
		t.Fatalf("identity substitution changed catalog: %+v %v", entries, err)
	}
	manifest.ID, manifest.Version = "fixture", "2.0.0"
	catalogTestWriteManifest(t, source, manifest)
	manager.ReleaseClient = managerReleaseFixtureClient("2.0.0", secondArchive)
	pinned, err := manager.Install(t.Context(), "github:fixture/module@v2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	manager.ReleaseClient = &http.Client{Transport: managerReleaseTransport(func(*http.Request) (*http.Response, error) {
		t.Error("a pinned or local package attempted network access")
		return nil, errors.New("network is forbidden")
	})}
	if err := manager.Restart(t.Context(), "fixture"); err != nil {
		t.Fatal("pinned package could not reload locally:", err)
	}
	entries, err = manager.List()
	if err != nil || len(entries) != 1 || entries[0].Directory == pinned.Directory ||
		entries[0].Source == nil || entries[0].Source.Reference != pinned.Source.Reference {
		t.Fatalf("reload lost the pin or failed to replace its generation: %+v %v", entries, err)
	}
	pinned = entries[0]
	unchanged, err = manager.Update(t.Context(), "fixture")
	if err != nil || unchanged.Revision != pinned.Revision {
		t.Fatalf("pinned release was changed: %+v %v", unchanged, err)
	}
	local, err := manager.Install(t.Context(), source)
	if err != nil || local.Source != nil {
		t.Fatalf("local replacement retained a release source: %+v %v", local, err)
	}
	unchanged, err = manager.Update(t.Context(), "fixture")
	if err != nil || unchanged.Revision != local.Revision {
		t.Fatalf("local package was changed: %+v %v", unchanged, err)
	}
	if _, err := manager.Update(t.Context(), "missing"); err == nil {
		t.Fatal("update accepted a nonexistent module")
	}
}

func TestManagerReleaseInstallAndUpdateUsePluginDownloadProxy(t *testing.T) {
	before := settings.Load()
	t.Cleanup(func() {
		if err := settings.Save(before); err != nil {
			t.Error(err)
		}
	})
	prefs := before
	prefs.PluginProxy = "socks5://127.0.0.1:15461"
	if err := settings.Save(prefs); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { managerReleaseClose(t, manager) })
	source := catalogTestPackage(t, "sample", "1.0.0")
	provenance := PackageSource{
		Reference: "github:fixture/module", Tag: "v1.0.0",
		Archive: "magpie-plugin_darwin_arm64.zip", SHA256: strings.Repeat("a", 64),
	}
	if _, err := manager.catalog.install(t.Context(), source, catalogTestCheck, installOptions{source: &provenance}); err != nil {
		t.Fatal(err)
	}
	intercepted := errors.New("download intercepted without dialing")
	calls := 0
	manager.ReleaseClient = &http.Client{Transport: managerReleaseTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		if choice := netproxy.Choice(request.Context()); choice != prefs.PluginProxy {
			t.Errorf("release request used %q instead of plugin proxy %q", choice, prefs.PluginProxy)
		}
		return nil, intercepted
	})}
	if _, err := manager.Install(t.Context(), "github:fixture/module"); !errors.Is(err, intercepted) {
		t.Fatalf("installation did not reach the controlled download: %v", err)
	}
	if _, err := manager.Update(t.Context(), "sample"); !errors.Is(err, intercepted) {
		t.Fatalf("update did not reach the controlled download: %v", err)
	}
	if calls != 2 {
		t.Fatalf("download paths were not both checked: %d", calls)
	}
}

func TestManagerReleaseUpdateDoesNotResurrectRemovedModule(t *testing.T) {
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { managerReleaseClose(t, manager) })
	source := fixturePackage(t)
	archive := managerReleaseArchive(t, source)
	manager.ReleaseClient = managerReleaseFixtureClient("1.0.0", archive)
	if _, err := manager.Install(t.Context(), "github:fixture/module"); err != nil {
		t.Fatal(err)
	}
	client := managerReleaseFixtureClient("1.0.0", archive)
	started, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	manager.ReleaseClient = &http.Client{Transport: managerReleaseTransport(func(request *http.Request) (*http.Response, error) {
		once.Do(func() { close(started) })
		select {
		case <-proceed:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		return client.Transport.RoundTrip(request)
	})}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := manager.Update(ctx, "fixture"); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("update did not reach release request:", ctx.Err())
	}
	if err := manager.Remove(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	close(proceed)
	select {
	case err := <-done:
		if !errors.Is(err, ErrCatalogConflict) {
			t.Fatalf("update lost the source-generation precondition: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("update did not finish")
	}
	entries, err := manager.List()
	if err != nil || len(entries) != 0 {
		t.Fatalf("update resurrected the removed module: %+v %v", entries, err)
	}
}

func TestCatalogReleaseMetadataKeepsLocalRevisionCompatibility(t *testing.T) {
	catalog := NewCatalog(t.TempDir())
	entry := catalogTestInstall(t, catalog, catalogTestPackage(t, "sample", "1.0.0"))
	// This is the catalog shape before optional release provenance existed.
	legacy := struct {
		Manifest  sdk.Manifest      `json:"manifest"`
		Directory string            `json:"directory"`
		Files     map[string]string `json:"files"`
		Disabled  bool              `json:"disabled"`
		Revision  string            `json:"revision"`
	}{Manifest: entry.Manifest, Directory: entry.Directory, Files: entry.Files}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%x", sha256.Sum256(data)); entry.Revision != want {
		t.Fatalf("old catalog revisions changed: got %s, want %s", entry.Revision, want)
	}
	valid := PackageSource{
		Reference: "github:fixture/module", Tag: "v1.0.0",
		Archive: "magpie-plugin_darwin_arm64.zip", SHA256: strings.Repeat("a", 64),
	}
	entry.Source = &valid
	entry.Revision = catalogRevision(entry)
	if err := catalogValidate(entry); err != nil {
		t.Fatal("valid source rejected:", err)
	}
	for _, mutate := range []func(*PackageSource){
		func(source *PackageSource) { source.Reference = "github:../module" },
		func(source *PackageSource) { source.Reference = "github:fixture/module@v2.0.0" },
		func(source *PackageSource) { source.Tag = "v2.0.0" },
		func(source *PackageSource) { source.Archive = "../outside.zip" },
		func(source *PackageSource) { source.SHA256 = "not-a-hash" },
	} {
		changed := valid
		mutate(&changed)
		entry.Source = &changed
		entry.Revision = catalogRevision(entry)
		if err := catalogValidate(entry); err == nil {
			t.Fatalf("invalid frozen source accepted: %+v", changed)
		}
	}
}

func TestReleaseHTTPRedirectPolicy(t *testing.T) {
	client := releaseHTTPClient()
	defer client.CloseIdleConnections()
	for _, address := range []string{
		"http://github.com/fixture", "https://user:secret@github.com/fixture", "https://github.com/fixture#fragment",
	} {
		request, err := http.NewRequest(http.MethodGet, address, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.CheckRedirect(request, nil); err == nil {
			t.Fatalf("unsafe redirect accepted: %s", strings.Split(address, "@")[0])
		}
	}
	request, _ := http.NewRequest(http.MethodGet, "https://release-assets.githubusercontent.com/fixture", nil)
	if err := client.CheckRedirect(request, nil); err != nil {
		t.Fatal("valid HTTPS asset redirect refused:", err)
	}
	if err := client.CheckRedirect(request, make([]*http.Request, 10)); err == nil {
		t.Fatal("redirect loop is unbounded")
	}
}
