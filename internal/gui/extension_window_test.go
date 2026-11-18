package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type windowTestHost struct {
	*extension.Manager
	call  func(context.Context, string, string, string, string, json.RawMessage) (json.RawMessage, error)
	ready func(context.Context, string, string, string, string) (extension.WindowServiceTarget, func(), error)
}

func (h *windowTestHost) CallWindowBinding(ctx context.Context, id, rev, part, binding string, input json.RawMessage) (json.RawMessage, error) {
	if h.call != nil {
		return h.call(ctx, id, rev, part, binding, input)
	}
	return h.Manager.CallWindowBinding(ctx, id, rev, part, binding, input)
}

func (h *windowTestHost) ReadyWindowServiceTarget(ctx context.Context, id, rev, part, binding string) (extension.WindowServiceTarget, func(), error) {
	if h.ready != nil {
		return h.ready(ctx, id, rev, part, binding)
	}
	return h.Manager.ReadyWindowServiceTarget(ctx, id, rev, part, binding)
}

func windowTestManager(t *testing.T) (*windowTestHost, *extension.Catalog) {
	t.Helper()
	root := t.TempDir()
	manager := extension.NewManager(root)
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	host := &windowTestHost{Manager: manager}
	old := extensionWindows
	extensionWindows = func() extensionWindowHost { return host }
	t.Cleanup(func() { extensionWindows = old })
	return host, extension.NewCatalog(root)
}

func windowTestInstall(t *testing.T, catalog *extension.Catalog, version string) extension.Installed {
	t.Helper()
	source := t.TempDir()
	manifest := sdk.Manifest{Schema: 1, ID: "our.module", Version: version, Runtime: "exec", Entry: "bin/worker",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 1}, Capabilities: []sdk.Capability{sdk.ModuleRPC}},
		Window:   &sdk.WindowDescriptor{Metadata: "package.json"},
		Actions: []sdk.ActionDescriptor{
			{ID: "refresh", InputSchema: json.RawMessage(`{"type":"object"}`), Effect: "read"},
			{ID: "side.read", Component: "sidecar", InputSchema: json.RawMessage(`{"type":"object"}`), Effect: "read"},
		},
		Services: []sdk.ServiceDescriptor{{ID: "sidecar", Runtime: "exec", Entry: "bin/service",
			Contract: sdk.ServiceContract{ID: "fixture-http", Version: 1}, Methods: []string{"side.read"}, Start: "lazy",
			Ready: sdk.ServiceReady{TimeoutMs: 1000, HTTPEndpoints: []string{"ui"}}}},
		Bindings: []sdk.BindingDescriptor{
			{ID: "page.refresh", From: "window:page", Action: "refresh"},
			{ID: "panel.refresh", From: "window:panel:0", Action: "refresh"},
			{ID: "page.service", From: "window:page", Service: "sidecar", Endpoint: "ui"},
		}}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		sdk.ManifestFile: string(body),
		"bin/worker":     "fixture primary must never execute during discovery",
		"bin/service":    "fixture service must never execute during discovery",
		"package.json": `{"name":"root-package","main":"legacy-must-not-load.js","magpie":{
			"page":{"title":"Composite","file":"ui/index.html","icon":"ui/icon.svg","service":{"port":9999}},
			"panels":[{"slot":"unknown","file":"ui/panel.html"},{"slot":"agents","file":"ui/panel.html","title":"Panel"}],
			"actions":[{"id":"open","menu":"provider"}],"permissions":["providers:list","not-a-public-read"]}}`,
		"ui/index.html": `<!doctype html><script src="_magpie/bridge.js"></script><script src="app.js"></script><p>version ` + version + `</p>`,
		"ui/panel.html": `<p>panel</p>`,
		"ui/app.js":     `window.FIXTURE_CHECKED="` + version + `";`,
		"ui/icon.svg":   `<svg xmlns="http://www.w3.org/2000/svg"/>`,
		"private.txt":   "not inside the window's folder",
	} {
		file := filepath.Join(source, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(data), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A catalog-only installation exercises real frozen files without loading a
	// backend. Runtime/ABI handshakes belong to the core integration tests.
	entry, err := catalog.Install(t.Context(), source, func(context.Context, extension.Installed) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func windowTestPages(t *testing.T, handler http.Handler) ([]pluginPage, []pluginPanel) {
	t.Helper()
	var pages []pluginPage
	var panels []pluginPanel
	for route, out := range map[string]any{"/api/plugin-pages": &pages, "/api/plugin-panels": &panels} {
		w := do(handler, httptest.NewRequest("GET", route, nil))
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), out) != nil {
			t.Fatalf("%s: %d %s", route, w.Code, w.Body)
		}
	}
	return pages, panels
}

func TestExtensionWindowCatalogAssets(t *testing.T) {
	pagePlugins(t)
	host, catalog := windowTestManager(t)
	entry := windowTestInstall(t, catalog, "1.0.0")
	var warmed atomic.Int32
	old := warmHost
	warmHost = func(context.Context) error { warmed.Add(1); return errors.New("legacy host must not load SDK modules") }
	t.Cleanup(func() { warmHost = old })
	for _, web := range []bool{false, true} {
		mux := http.NewServeMux()
		pluginPageRoutes(mux, web)
		var handler http.Handler = mux
		if !web {
			handler = ownWindow(mux)
		}
		pages, panels := windowTestPages(t, handler)
		var page pluginPage
		for _, p := range pages {
			if p.Ref != "" {
				page = p
			}
		}
		if page.Ref == "" || page.hosted || page.Key != "extension-6f75722e6d6f64756c65" || len(panels) != 1 {
			t.Fatalf("SDK page/panel discovery: %+v %+v", page, panels)
		}
		if page.Service != pageServicePath+"page.service/" || page.Spec == entry.Manifest.ID || len(page.Reads) != 1 {
			t.Fatalf("SDK authority/proxy/reads missing: %+v", page)
		}
		if page.Ref == panels[0].Ref || panels[0].Key != page.Key+"-0" {
			t.Fatal("page and filtered-index panel did not get separate references")
		}
		for _, rel := range []string{"index.html", "app.js", pluginBridgePath} {
			uri := strings.TrimSuffix(page.URL, "index.html") + rel
			w := do(handler, httptest.NewRequest("GET", uri, nil))
			if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox allow-scripts") {
				t.Fatalf("SDK %s: %d %s", rel, w.Code, w.Body)
			}
			if web && rel == "index.html" && !strings.Contains(w.Body.String(), `FIXTURE_CHECKED`) {
				t.Fatal("browser inlining bypassed the checked reader or lost the script")
			}
			scheme := "wails://"
			if runtime.GOOS == "windows" {
				scheme = "http://"
			}
			if !web && !strings.Contains(w.Header().Get("Content-Security-Policy"), scheme) {
				t.Fatal("native transport lost its explicitly named CSP scheme")
			}
		}
		for _, rel := range []string{"../private.txt", "link.txt", "/private.txt"} {
			r := httptest.NewRequest("GET", strings.TrimSuffix(page.URL, "index.html")+rel, nil)
			w := httptest.NewRecorder()
			servePluginPage(w, r, web)
			if w.Code == 200 {
				t.Fatalf("SDK window served an outside or unknown file %q", rel)
			}
		}
	}
	if warmed.Load() != 0 || host.Running(entry.Manifest.ID) {
		t.Fatal("SDK metadata/static assets started a backend")
	}
	// The only legacy entries are the unrelated fixtures: discovery never
	// registers the SDK directory in plugins.json.
	data, err := os.ReadFile(filepath.Join(settings.Dir(), "plugins.json"))
	if err != nil || strings.Contains(string(data), "our.module") {
		t.Fatalf("SDK discovery changed plugins.json: %s %v", data, err)
	}
}

func TestExtensionWindowRevisionsAndUnknownCatalog(t *testing.T) {
	pagePlugins(t)
	host, catalog := windowTestManager(t)
	windowTestInstall(t, catalog, "1.0.0")
	mux := pageMux()
	pages, _, err := extensionWindowParts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 {
		t.Fatal("missing SDK page")
	}
	first := pages[0]
	if _, err := host.CompareAndSwapModuleConfig(t.Context(), "our.module", "0", json.RawMessage(`{"label":"new"}`)); err != nil {
		t.Fatal(err)
	}
	pages, _, err = extensionWindowParts(t.Context())
	if err != nil || len(pages) != 1 || pages[0].Ref == first.Ref || pages[0].URL == first.URL {
		t.Fatalf("config generation did not replace the view reference: %+v %v", pages, err)
	}
	if pages[0].Key != first.Key {
		t.Fatal("configuration changed the stable navigation key")
	}
	for _, p := range []pluginPage{first, pages[0]} {
		if p.Ref == first.Ref && do(mux, httptest.NewRequest("GET", p.URL, nil)).Code == 200 {
			t.Fatal("old configuration ref served current assets")
		}
	}
	beforeUpdate := pages[0]
	entry := windowTestInstall(t, catalog, "2.0.0")
	pages, _, err = extensionWindowParts(t.Context())
	if err != nil || len(pages) != 1 || pages[0].Ref == beforeUpdate.Ref {
		t.Fatalf("package update retained stale ref: %+v %v", pages, err)
	}
	if pages[0].Key != first.Key {
		t.Fatal("package update changed the stable navigation key")
	}
	if do(mux, httptest.NewRequest("GET", beforeUpdate.URL, nil)).Code == 200 {
		t.Fatal("old package ref retargeted to new assets")
	}
	if err := catalog.SetEnabled(t.Context(), "our.module", false); err != nil {
		t.Fatal(err)
	}
	if do(mux, httptest.NewRequest("GET", pages[0].URL, nil)).Code == 200 {
		t.Fatal("disabled module's assets remain served")
	}
	if err := catalog.SetEnabled(t.Context(), "our.module", true); err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(catalog.Root, filepath.FromSlash(entry.Directory), "ui", "app.js")
	if err := os.WriteFile(asset, []byte(`window.TAMPERED=true`), 0o600); err != nil {
		t.Fatal(err)
	}
	if do(mux, httptest.NewRequest("GET", strings.TrimSuffix(pages[0].URL, "index.html")+"app.js", nil)).Code == 200 {
		t.Fatal("SDK assets bypassed catalog hash validation")
	}
	if err := os.WriteFile(filepath.Join(catalog.Root, "catalog.json"), []byte(`{"cut":`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/api/plugin-pages", "/api/plugin-panels"} {
		w := do(mux, httptest.NewRequest("GET", route, nil))
		if w.Code < 400 || strings.TrimSpace(w.Body.String()) == "[]" {
			t.Fatalf("unknown catalog was reported as an empty list: %s %d %s", route, w.Code, w.Body)
		}
	}
}

func TestExtensionWindowBindingsAndConfig(t *testing.T) {
	pagePlugins(t)
	host, catalog := windowTestManager(t)
	windowTestInstall(t, catalog, "1.0.0")
	pages, panels, err := extensionWindowParts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	page, panel := pages[0], panels[0]
	var called int
	host.call = func(ctx context.Context, id, revision, part, binding string, input json.RawMessage) (json.RawMessage, error) {
		called++
		if id != "our.module" || revision == "" || string(input) != `{"value":3}` {
			t.Fatalf("frame supplied backend authority: %q %q %s", id, revision, input)
		}
		if part == "window:page" && binding != "page.refresh" || part == "window:panel:0" && binding != "panel.refresh" {
			return nil, errors.New("binding not declared for this part")
		}
		return json.RawMessage(`{"done":true}`), nil
	}
	mux := pageMux()
	post := func(route, body string) *httptest.ResponseRecorder {
		return do(mux, httptest.NewRequest("POST", route, strings.NewReader(body)))
	}
	for ref, binding := range map[string]string{page.Ref: "page.refresh", panel.Ref: "panel.refresh"} {
		w := post("/api/plugin-pages/call", `{"ref":"`+ref+`","binding":"`+binding+`","input":{"value":3},"module":"forged","revision":"forged","scope":{"account":"secret"}}`)
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"done":true}` {
			t.Fatalf("declared binding: %d %s", w.Code, w.Body)
		}
	}
	if called != 2 {
		t.Fatalf("backend calls: %d", called)
	}
	if post("/api/plugin-pages/call", `{"ref":"`+page.Ref+`","binding":"page.refresh","request":"`+strings.Repeat("x", 257)+`","input":{"value":3}}`).Code == 200 {
		t.Fatal("unbounded replay identity reached the backend")
	}
	if called != 2 {
		t.Fatal("invalid replay identity invoked a backend")
	}
	w := post("/api/plugin-pages/call", `{"ref":"`+panel.Ref+`","binding":"page.refresh","input":{"value":3}}`)
	if w.Code == 200 {
		t.Fatal("panel borrowed page authority")
	}
	r := httptest.NewRequest("POST", "/api/plugin-pages/call", strings.NewReader(`{"ref":"`+page.Ref+`","binding":"page.refresh","input":{"value":3}}`))
	r.Header.Set("Origin", "null")
	if do(mux, r).Code != http.StatusForbidden {
		t.Fatal("a frame directly reached the backend API")
	}
	w = post("/api/plugin-pages/config", `{"ref":"`+page.Ref+`"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"version":"0"`) {
		t.Fatalf("configuration read: %d %s", w.Code, w.Body)
	}
	for _, body := range []string{
		`{"ref":"` + page.Ref + `","values":{"label":"bad"}}`,
		`{"ref":"` + page.Ref + `","expected":"0"}`,
	} {
		if post("/api/plugin-pages/config", body).Code == 200 {
			t.Fatal("configuration mutation did not require explicit CAS version and values")
		}
	}
	w = post("/api/plugin-pages/config", `{"ref":"`+page.Ref+`","expected":"0","values":{"label":"new"}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"version":"1"`) {
		t.Fatalf("configuration CAS: %d %s", w.Code, w.Body)
	}
	if post("/api/plugin-pages/config", `{"ref":"`+page.Ref+`"}`).Code == 200 {
		t.Fatal("old configuration reference remained usable")
	}
	if post("/api/plugin-pages/call", `{"ref":"`+page.Ref+`","binding":"page.refresh","input":{"value":3}}`).Code == 200 {
		t.Fatal("old window called the new runtime configuration")
	}
}

func TestExtensionWindowServiceLeaseAndProxy(t *testing.T) {
	pagePlugins(t)
	host, catalog := windowTestManager(t)
	windowTestInstall(t, catalog, "1.0.0")
	pages, _, err := extensionWindowParts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	part := pages[0].window
	requested, finish := make(chan struct{}), make(chan struct{})
	var released atomic.Int32
	var acquired atomic.Int32
	proof := extension.WindowServiceTarget{Instance: "fixture-instance", Component: "sidecar", ConfigVersion: "0"}
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/owned/events" || r.Header.Get("Cookie") != "" || r.Host == "" {
			t.Errorf("proxy request: %s, cookie %q", r.URL.Path, r.Header.Get("Cookie"))
		}
		for _, name := range []string{sdk.HeaderComponentInstance, sdk.HeaderComponent, sdk.HeaderComponentConfig} {
			if len(r.Header.Values(name)) != 0 {
				t.Errorf("client proof header reached SDK service: %s", name)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "must-not-escape=true")
		w.Header().Set("Content-Security-Policy", "connect-src 'self'")
		module := sdk.ModuleContext{Instance: proof.Instance, Component: proof.Component, Config: sdk.ConfigSnapshot{Version: proof.ConfigVersion}}
		for name, values := range module.HTTPHeaders() {
			w.Header()[name] = values
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(requested)
		select {
		case <-finish:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(service.Close)
	host.ready = func(ctx context.Context, id, revision, kind, binding string) (extension.WindowServiceTarget, func(), error) {
		acquired.Add(1)
		if id != part.id || revision != part.revision || kind != "window:page" || binding != "page.service" {
			return extension.WindowServiceTarget{}, nil, errors.New("undeclared endpoint")
		}
		target := proof
		target.URL = service.URL + "/owned/"
		return target, func() { released.Add(1) }, nil
	}
	for _, rel := range []string{pageServicePath + "page.service/../private", pageServicePath + "page.service/events"} {
		req := httptest.NewRequest("GET", pluginPagePrefix+part.ref+"/"+rel, nil)
		if strings.HasSuffix(rel, "events") {
			req.Header.Set("Origin", "https://foreign.invalid")
		}
		w := httptest.NewRecorder()
		serveExtensionWindowService(w, req, part, rel)
		if w.Code < 400 {
			t.Fatal("foreign origin or outside endpoint path reached a service")
		}
	}
	if acquired.Load() != 0 {
		t.Fatal("invalid service request started a backend")
	}
	mux := http.NewServeMux()
	pluginPageRoutes(mux, false) // native and browser use this same token proxy
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	uri := server.URL + pluginPagePrefix + part.ref + "/" + pageServicePath + "page.service/events"
	req, _ := http.NewRequestWithContext(t.Context(), "GET", uri, nil)
	req.Header.Set("Cookie", "window=secret")
	req.Header.Set("Origin", "null")
	for _, name := range []string{sdk.HeaderComponentInstance, sdk.HeaderComponent, sdk.HeaderComponentConfig} {
		req.Header.Set(name, "browser-supplied")
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		close(finish)
		t.Fatal(err)
	}
	<-requested
	if released.Load() != 0 {
		t.Fatal("service lease released before its event stream finished")
	}
	if resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("X-Accel-Buffering") != "no" || !strings.Contains(strings.Join(resp.Header.Values("Content-Security-Policy"), ";"), pageServicePolicy) {
		t.Fatalf("service proxy policy/cookies: %v", resp.Header)
	}
	for _, name := range []string{sdk.HeaderComponentInstance, sdk.HeaderComponent, sdk.HeaderComponentConfig} {
		if len(resp.Header.Values(name)) != 0 {
			t.Fatalf("service proof header leaked to the frontend: %s", name)
		}
	}
	close(finish)
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	// The handler's deferred release precedes the fully drained response.
	server.Close()
	if released.Load() != 1 {
		t.Fatalf("service lease releases: %d", released.Load())
	}
	target, _ := url.Parse(service.URL + "/owned/")
	if got := windowServiceLocation(service.URL+"/owned/next?q=1", target); got != "/next?q=1" {
		t.Fatalf("endpoint redirect escaped token root: %s", got)
	}
	for _, invalid := range []string{"ftp://127.0.0.1:1234/", "http://example.com:1234/", "http://127.0.0.1:1234/?token=secret", "http://user@127.0.0.1:1234/"} {
		target, _ := url.Parse(invalid)
		if windowServiceTarget(target) {
			t.Fatalf("unowned endpoint accepted: %s", invalid)
		}
	}
}

func TestExtensionWindowServiceProof(t *testing.T) {
	pagePlugins(t)
	host, catalog := windowTestManager(t)
	windowTestInstall(t, catalog, "1.0.0")
	pages, _, err := extensionWindowParts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	part := pages[0].window
	proof := extension.WindowServiceTarget{Instance: "current-instance", Component: "sidecar", ConfigVersion: part.config.Version}
	reserved := []string{sdk.HeaderComponentInstance, sdk.HeaderComponent, sdk.HeaderComponentConfig}
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range reserved {
			if len(r.Header.Values(name)) != 0 {
				t.Errorf("browser proof forwarded to backend: %s", name)
			}
		}
		module := sdk.ModuleContext{Instance: proof.Instance, Component: proof.Component, Config: sdk.ConfigSnapshot{Version: proof.ConfigVersion}}
		for name, values := range module.HTTPHeaders() {
			w.Header()[name] = values
		}
		mode := strings.TrimPrefix(r.URL.Path, "/owned/")
		switch mode {
		case "wrong-instance":
			w.Header().Set(sdk.HeaderComponentInstance, "old-instance")
		case "wrong-component":
			w.Header().Set(sdk.HeaderComponent, "different-service")
		case "wrong-config":
			w.Header().Set(sdk.HeaderComponentConfig, "different-config")
		case "missing-instance":
			w.Header().Del(sdk.HeaderComponentInstance)
		case "missing-component":
			w.Header().Del(sdk.HeaderComponent)
		case "missing-config":
			w.Header().Del(sdk.HeaderComponentConfig)
		case "duplicate-instance":
			w.Header().Add(sdk.HeaderComponentInstance, proof.Instance)
		case "empty-instance":
			w.Header().Set(sdk.HeaderComponentInstance, "")
		case "missing-all", "":
			for _, name := range reserved {
				w.Header().Del(name)
			}
		}
		w.Header().Set("X-Upstream-Sentinel", "private")
		w.Header().Set("Set-Cookie", "private-cookie=true")
		if mode == "matching-trailers" {
			w.Header().Set("Trailer", "X-Useful")
		}
		_, _ = io.WriteString(w, "UPSTREAM_PRIVATE_BODY:"+mode)
		if mode == "matching-trailers" {
			w.Header().Set("X-Useful", "kept")
			for _, name := range reserved {
				w.Header().Set(http.TrailerPrefix+name, "late-private-proof")
			}
		}
	}))
	t.Cleanup(service.Close)
	var released atomic.Int32
	host.ready = func(ctx context.Context, id, revision, kind, binding string) (extension.WindowServiceTarget, func(), error) {
		if id != part.id || revision != part.revision || kind != part.part || binding != "page.service" {
			return extension.WindowServiceTarget{}, nil, errors.New("wrong service authority")
		}
		target := proof
		target.URL = service.URL + "/owned/"
		return target, func() { released.Add(1) }, nil
	}
	for _, mode := range []string{"matching", "matching-trailers", "wrong-instance", "wrong-component", "wrong-config", "missing-instance", "missing-component", "missing-config", "missing-all", "duplicate-instance", "empty-instance", ""} {
		name := mode
		if name == "" {
			name = "root-missing-proof"
		}
		t.Run(name, func(t *testing.T) {
			rel := pageServicePath + "page.service/" + mode
			req := httptest.NewRequest("GET", pluginPagePrefix+part.ref+"/"+rel, nil)
			req.Header.Set("Origin", "null")
			// Even knowing a matching value must not let the browser stamp
			// response authority through a backend that echoes request headers.
			req.Header.Set(sdk.HeaderComponentInstance, proof.Instance)
			req.Header.Set(sdk.HeaderComponent, proof.Component)
			req.Header.Set(sdk.HeaderComponentConfig, proof.ConfigVersion)
			before := released.Load()
			w := httptest.NewRecorder()
			serveExtensionWindowService(w, req, part, rel)
			if released.Load() != before+1 {
				t.Fatal("service lease was not released after the proxy")
			}
			for _, header := range reserved {
				if len(w.Header().Values(header)) != 0 || len(w.Result().Trailer.Values(header)) != 0 {
					t.Fatalf("service proof leaked to frontend: %s", header)
				}
			}
			if mode == "matching" || mode == "matching-trailers" {
				if w.Code != 200 || w.Body.String() != "UPSTREAM_PRIVATE_BODY:"+mode {
					t.Fatalf("matching generation rejected: %d %s", w.Code, w.Body)
				}
				if mode == "matching-trailers" && w.Result().Trailer.Get("X-Useful") != "kept" {
					t.Fatal("ordinary service trailer was removed")
				}
			} else if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "UPSTREAM_PRIVATE_BODY") || w.Header().Get("X-Upstream-Sentinel") != "" {
				t.Fatalf("unproved generation admitted headers/body: %d %v %s", w.Code, w.Header(), w.Body)
			}
		})
	}
}

func TestPluginPageServiceProofRemainsLegacyOptional(t *testing.T) {
	reserved := []string{sdk.HeaderComponentInstance, sdk.HeaderComponent, sdk.HeaderComponentConfig}
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range reserved {
			if r.Header.Get(name) != "legacy-browser" {
				t.Errorf("legacy request header changed: %s=%q", name, r.Header.Get(name))
			}
			w.Header().Set(name, "legacy-service")
		}
		_, _ = io.WriteString(w, "legacy body")
	}))
	t.Cleanup(service.Close)
	endpoint, err := url.Parse(service.URL)
	if err != nil {
		t.Fatal(err)
	}
	root := pluginPagePrefix + pageToken("legacy-proof") + "/" + pageServicePath
	req := httptest.NewRequest("GET", root+"read", nil)
	for _, name := range reserved {
		req.Header.Set(name, "legacy-browser")
	}
	w := httptest.NewRecorder()
	servePageServiceTarget(w, req, "legacy", endpoint, root, nil)
	if w.Code != 200 || w.Body.String() != "legacy body" {
		t.Fatalf("legacy service required SDK proof: %d %s", w.Code, w.Body)
	}
	for _, name := range reserved {
		if w.Header().Get(name) != "legacy-service" {
			t.Fatalf("legacy response header changed: %s=%q", name, w.Header().Get(name))
		}
	}
}

type windowProofDuplex struct{ bytes.Buffer }

func (*windowProofDuplex) Close() error { return nil }

func TestExtensionWindowServiceProofKeepsUpgradeDuplex(t *testing.T) {
	module := sdk.ModuleContext{Instance: "upgrade-instance", Component: "sidecar", Config: sdk.ConfigSnapshot{Version: "4"}}
	connection := &windowProofDuplex{}
	response := &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: module.HTTPHeaders(), Body: connection}
	proof := extension.WindowServiceTarget{Instance: module.Instance, Component: module.Component, ConfigVersion: module.Config.Version}
	if err := verifyWindowServiceProof(response, &proof); err != nil {
		t.Fatal(err)
	}
	if response.Body != connection {
		t.Fatal("proof filtering replaced the upgraded duplex connection")
	}
	if _, ok := response.Body.(io.ReadWriteCloser); !ok {
		t.Fatal("proof filtering removed the upgraded connection's writer")
	}
	for _, name := range []string{sdk.HeaderComponentInstance, sdk.HeaderComponent, sdk.HeaderComponentConfig} {
		if len(response.Header.Values(name)) != 0 {
			t.Fatalf("upgrade proof header leaked: %s", name)
		}
	}
}

func TestExtensionWindowChangeReadAcrossProcesses(t *testing.T) {
	pagePlugins(t)
	host, catalog := windowTestManager(t)
	windowTestInstall(t, catalog, "1.0.0")
	before, err := extensionWindowVersion(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	mux := pageMux()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			done <- do(mux, httptest.NewRequest("GET", "/api/plugin-pages/changes?version="+before, nil).WithContext(ctx))
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("unchanged window metadata did not wait")
		default:
		}
		// This independent manager writes the same store without notifying this
		// GUI process. The next bounded metadata read must detect the version.
		other := extension.NewManager(catalog.Root)
		defer other.Close(context.Background())
		if _, err := other.CompareAndSwapModuleConfig(ctx, "our.module", "0", json.RawMessage(`{"external":true}`)); err != nil {
			t.Fatal(err)
		}
		result := <-done
		var changed struct{ Version string }
		if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &changed) != nil || changed.Version == before {
			t.Fatalf("external CAS notification: %d %s", result.Code, result.Body)
		}
	})
	if host.Running("our.module") {
		t.Fatal("window changes started a backend")
	}
	if err := catalog.SetEnabled(t.Context(), "our.module", false); err != nil {
		t.Fatal(err)
	}
	after, err := extensionWindowVersion(t.Context())
	if err != nil || after == before {
		t.Fatalf("external disable did not change window metadata: %q %v", after, err)
	}
	if err := os.WriteFile(filepath.Join(catalog.Root, "catalog.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := do(mux, httptest.NewRequest("GET", "/api/plugin-pages/changes?version="+after, nil)); result.Code < 400 {
		t.Fatalf("unknown catalog notification reported success: %d %s", result.Code, result.Body)
	}
}
