//go:build cgo && (darwin || linux || freebsd || windows)

package gui

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hybrid "github.com/yetone/magpie/examples/extensions/hybrid-go"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var windowHybridDistribution = flag.String("window-hybrid-distribution", "", "parent-owned hybrid distribution")
var windowHybridRoot = flag.String("window-hybrid-root", "", "parent-owned hybrid catalog")

const windowHybridID = "sdk-hybrid-go"

func windowHybridSourceRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate hybrid example source")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(filename), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func windowHybridBuild(t *testing.T) string {
	t.Helper()
	// pagePlugins clears PATH and changes HOME. Resolve author tools and pin
	// caches before that fixture is installed; execution needs none of them.
	tools := map[string]string{}
	for _, name := range []string{"go", "python3", "tsc"} {
		filename, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("hybrid build requires an installed %s: %v", name, err)
		}
		tools[name] = filename
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	command := proc.CommandContext(ctx, tools["go"], "env", "-json", "GOPATH", "GOMODCACHE", "GOCACHE")
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var caches map[string]string
	if err := json.Unmarshal(output, &caches); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "distribution")
	sourceRoot := windowHybridSourceRoot(t)
	command = proc.CommandContext(ctx, tools["python3"], filepath.Join(sourceRoot, "examples", "extensions", "hybrid-go", "build.py"), destination)
	command.Dir = sourceRoot
	command.Env = append(os.Environ(),
		"GOPATH="+caches["GOPATH"], "GOMODCACHE="+caches["GOMODCACHE"], "GOCACHE="+caches["GOCACHE"],
		"GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=1", "PYTHONDONTWRITEBYTECODE=1",
		"PATH="+filepath.Dir(tools["go"])+string(os.PathListSeparator)+filepath.Dir(tools["tsc"])+string(os.PathListSeparator)+os.Getenv("PATH"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build public hybrid example: %v\n%s", err, output)
	}
	return destination
}

func TestExtensionWindowHybridRuntime(t *testing.T) {
	distribution, root := *windowHybridDistribution, *windowHybridRoot
	if distribution == "" {
		distribution, root = windowHybridBuild(t), t.TempDir()
		if runtime.GOOS == "windows" {
			// Native libraries remain mapped until exit. The parent owns their
			// directories so Windows cleanup happens after this host has exited.
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			command := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestExtensionWindowHybridRuntime$", "-test.v",
				"-window-hybrid-distribution="+distribution, "-window-hybrid-root="+root)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("hybrid GUI Windows host: %v\n%s", err, output)
			} else {
				t.Log(string(output))
			}
			return
		}
	}
	if root == "" {
		t.Fatal("hybrid window fixture requires a parent-owned catalog")
	}
	pagePlugins(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	manager := extension.NewManager(root)
	manager.Bun = filepath.Join(root, "no-bun-runtime")
	t.Cleanup(func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := manager.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	oldHost, oldWarm := extensionWindows, warmHost
	var warmed atomic.Int32
	extensionWindows = func() extensionWindowHost { return manager }
	warmHost = func(context.Context) error {
		warmed.Add(1)
		return errors.New("SDK window unexpectedly started the legacy host")
	}
	t.Cleanup(func() { extensionWindows, warmHost = oldHost, oldWarm })
	entry, err := manager.Install(ctx, distribution)
	if err != nil {
		t.Fatal(err)
	}
	handler := pageMux()
	post := func(route string, body any) *httptest.ResponseRecorder {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, route, bytes.NewReader(data)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		return do(handler, request)
	}
	findPage := func() (pluginPage, bool) {
		t.Helper()
		response := do(handler, httptest.NewRequest(http.MethodGet, "/api/plugin-pages", nil).WithContext(ctx))
		var pages []pluginPage
		windowHybridDecode(t, response, &pages)
		for _, page := range pages {
			if page.Name == windowHybridID && page.Ref != "" {
				return page, true
			}
		}
		return pluginPage{}, false
	}
	page, found := findPage()
	if !found || len(page.Ref) != 32 || page.Service != pageServicePath+"page.service/" {
		t.Fatalf("hybrid page/binding was not discovered: %+v", page)
	}
	base := strings.TrimSuffix(page.URL, "index.html")
	for relative, filename := range map[string]string{"index.html": "ui/index.html", "app.js": "ui/app.js"} {
		response := do(handler, httptest.NewRequest(http.MethodGet, base+relative, nil).WithContext(ctx))
		digest := sha256.Sum256(response.Body.Bytes())
		if response.Code != http.StatusOK || hex.EncodeToString(digest[:]) != entry.Files[filename] ||
			!strings.Contains(response.Header().Get("Content-Security-Policy"), "sandbox allow-scripts") {
			t.Fatalf("frozen hybrid asset %s: %d %s", relative, response.Code, response.Body)
		}
	}
	bridge := do(handler, httptest.NewRequest(http.MethodGet, base+pluginBridgePath, nil).WithContext(ctx))
	if bridge.Code != http.StatusOK || !strings.Contains(bridge.Body.String(), "backend.call") {
		t.Fatal("hybrid page did not receive the parent bridge")
	}
	if manager.Running(windowHybridID) || warmed.Load() != 0 {
		t.Fatal("discovery/static assets started a serving backend after the install probe")
	}
	legacy, err := os.ReadFile(filepath.Join(settings.Dir(), "plugins.json"))
	if err != nil || bytes.Contains(legacy, []byte(windowHybridID)) {
		t.Fatalf("SDK window entered the legacy plugin catalog: %s, %v", legacy, err)
	}
	call := func(ref, binding, request string, input any, out any) {
		t.Helper()
		response := post("/api/plugin-pages/call", map[string]any{
			"ref": ref, "binding": binding, "request": request, "input": input,
		})
		windowHybridDecode(t, response, out)
	}
	var primary, backend hybrid.Identity
	call(page.Ref, "page.pid", "", map[string]any{}, &primary)
	call(page.Ref, "page.backend", "", map[string]any{}, &backend)
	if primary.PID != os.Getpid() || backend.PID <= 0 || backend.PID == primary.PID ||
		primary.ConfigVersion != "0" || backend.ConfigVersion != primary.ConfigVersion {
		t.Fatalf("GUI bindings lost native/service architecture or shared configuration: %+v / %+v, host=%d", primary, backend, os.Getpid())
	}
	var forwarded hybrid.ForwardResult
	call(page.Ref, "page.forward", "one-window-write", map[string]string{"text": "once"}, &forwarded)
	if forwarded.Primary.PID != primary.PID || forwarded.Backend.PID != backend.PID || forwarded.Backend.Text != "once" {
		t.Fatalf("GUI write did not traverse the native controller and its helper: %+v", forwarded)
	}
	scope := sdk.Scope{Plugin: windowHybridID}
	before, err := manager.Store().StateGet(scope, "hybrid.last")
	if err != nil || before.Version != 1 || string(before.Value) != `{"text":"once"}` {
		t.Fatalf("forwarded write did not reach the real helper state: %+v, %v", before, err)
	}
	repeated := post("/api/plugin-pages/call", map[string]any{
		"ref": page.Ref, "binding": "page.forward", "request": "one-window-write", "input": map[string]string{"text": "replayed"},
	})
	after, err := manager.Store().StateGet(scope, "hybrid.last")
	if repeated.Code < 400 || err != nil || after.Version != before.Version || !bytes.Equal(after.Value, before.Value) {
		t.Fatalf("duplicate window request executed twice: HTTP=%d state=%+v err=%v", repeated.Code, after, err)
	}
	var initialConfig, configured sdk.ConfigSnapshot
	windowHybridDecode(t, post("/api/plugin-pages/config", map[string]string{"ref": page.Ref}), &initialConfig)
	windowHybridDecode(t, post("/api/plugin-pages/config", map[string]any{
		"ref": page.Ref, "expected": initialConfig.Version, "values": map[string]string{"value": "window-config"},
	}), &configured)
	if configured.Version == initialConfig.Version {
		t.Fatal("GUI configuration CAS did not create a new snapshot")
	}
	for route, body := range map[string]any{
		"/api/plugin-pages/config": map[string]string{"ref": page.Ref},
		"/api/plugin-pages/call":   map[string]any{"ref": page.Ref, "binding": "page.pid", "input": map[string]any{}},
	} {
		if response := post(route, body); response.Code < 400 {
			t.Fatalf("old frame retained authority after configuration CAS: %s", route)
		}
	}
	if response := do(handler, httptest.NewRequest(http.MethodGet, page.URL, nil).WithContext(ctx)); response.Code < 400 {
		t.Fatal("old frame retargeted to the new configuration's assets")
	}
	fresh, found := findPage()
	if !found || fresh.Ref == page.Ref || fresh.Key != page.Key {
		t.Fatalf("configuration CAS did not replace the view reference: before=%+v after=%+v", page, fresh)
	}
	var freshPrimary, freshBackend hybrid.Identity
	call(fresh.Ref, "page.pid", "", map[string]any{}, &freshPrimary)
	call(fresh.Ref, "page.backend", "", map[string]any{}, &freshBackend)
	for _, value := range []hybrid.Identity{freshPrimary, freshBackend} {
		if value.ConfigVersion != configured.Version || value.Value != "window-config" {
			t.Fatalf("fresh frame saw inconsistent runtime configuration: %+v", value)
		}
	}
	if freshPrimary.PID != os.Getpid() || freshPrimary.Instance == primary.Instance ||
		freshBackend.PID == backend.PID || freshBackend.Instance == backend.Instance {
		t.Fatalf("configuration CAS reused a component instance: %+v / %+v", freshPrimary, freshBackend)
	}
	// Observe actual bytes written by the real proxy to the real helper.
	// The observer delegates every connection and does not replace the service.
	observed := make(chan windowHybridHeaders, 4)
	oldTransport := pageServiceTransport
	transport := oldTransport.Clone()
	transport.DisableKeepAlives = true
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := windowHybridDial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &windowHybridConn{Conn: connection, observed: observed}, nil
	}
	pageServiceTransport = transport
	t.Cleanup(func() {
		pageServiceTransport = oldTransport
		transport.CloseIdleConnections()
	})
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		cancel()
		server.CloseClientConnections()
		server.Close()
	})
	clientTransport := &http.Transport{DialContext: windowHybridDial}
	t.Cleanup(clientTransport.CloseIdleConnections)
	client := &http.Client{Transport: clientTransport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("hybrid test refuses redirects")
	}}
	serviceURL := server.URL + strings.TrimSuffix(fresh.URL, "index.html") + fresh.Service
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, serviceURL+"status", nil)
	request.Header.Set("Cookie", "window-session=synthetic-private-cookie")
	request.Header.Set("Origin", "null")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var status hybrid.Identity
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&status)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil || status.PID != freshBackend.PID ||
		status.ConfigVersion != configured.Version || status.Value != "window-config" {
		t.Fatalf("real GUI service proxy lost component authority: %+v, HTTP=%d err=%v", status, response.StatusCode, decodeErr)
	}
	windowHybridProofHidden(t, response.Header)
	windowHybridCheckHeaders(t, ctx, observed, "/status")
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	request, _ = http.NewRequestWithContext(streamCtx, http.MethodGet, serviceURL+"events", nil)
	request.Header.Set("Cookie", "window-session=synthetic-private-cookie")
	request.Header.Set("Origin", "null")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	if response.StatusCode != http.StatusOK || !scanner.Scan() || scanner.Text() != "event: ready" || !scanner.Scan() {
		t.Fatal("real GUI proxy did not stream the helper's readiness event")
	}
	if !strings.HasPrefix(scanner.Text(), "data: ") ||
		json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &status) != nil ||
		status.PID != freshBackend.PID || status.ConfigVersion != configured.Version {
		t.Fatalf("GUI SSE came from another component generation: %q", scanner.Text())
	}
	if response.Header.Get("X-Accel-Buffering") != "no" ||
		!strings.Contains(strings.Join(response.Header.Values("Content-Security-Policy"), ";"), pageServicePolicy) {
		t.Fatalf("GUI SSE lost its streaming/sandbox policy: %v", response.Header)
	}
	windowHybridProofHidden(t, response.Header)
	windowHybridCheckHeaders(t, ctx, observed, "/events")
	streamDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, response.Body)
		close(streamDone)
	}()
	defer func() {
		stopStream()
		response.Body.Close()
		select {
		case <-streamDone:
		case <-time.After(5 * time.Second):
			t.Error("hybrid GUI stream reader did not stop")
		}
	}()
	if err := manager.SetEnabled(ctx, windowHybridID, false); err != nil {
		t.Fatal(err)
	}
	if disabled, found := findPage(); found {
		t.Fatalf("disabled module still appeared in page discovery: %+v", disabled)
	}
	for _, route := range []string{fresh.URL, strings.TrimSuffix(fresh.URL, "index.html") + "app.js",
		strings.TrimSuffix(fresh.URL, "index.html") + fresh.Service + "status"} {
		if result := do(handler, httptest.NewRequest(http.MethodGet, route, nil).WithContext(ctx)); result.Code < 400 {
			t.Fatalf("disabled frame reference retained access: %s", route)
		}
	}
	if result := post("/api/plugin-pages/call", map[string]any{
		"ref": fresh.Ref, "binding": "page.pid", "input": map[string]any{},
	}); result.Code < 400 {
		t.Fatal("disabled frame still called the runtime")
	}
	// Existing proxied SSE owns the old runtime lease even after withdrawal.
	request, _ = http.NewRequestWithContext(ctx, http.MethodGet, freshBackend.Endpoint+"/status", nil)
	retained, err := client.Do(request)
	if err != nil {
		t.Fatalf("disable disposed the service while its proxied SSE still owned a lease: %v", err)
	}
	retained.Body.Close()
	if retained.StatusCode != http.StatusOK {
		t.Fatal("in-flight GUI service lease did not retain its helper")
	}
	select {
	case <-streamDone:
		t.Fatal("disable terminated the existing GUI service stream")
	default:
	}
	stopStream()
	response.Body.Close()
	select {
	case <-streamDone:
	case <-ctx.Done():
		t.Fatal("canceled GUI SSE did not release its reader")
	}
	windowHybridAwaitClosed(t, ctx, freshBackend.Endpoint)
	if warmed.Load() != 0 {
		t.Fatal("SDK window entered the legacy Bun host")
	}
	t.Logf("GUI binding nativePID=%d equals hostPID=%d; helperPID=%d; config CAS %s->%s; replacement helperPID=%d; SSE retained then released the disabled generation",
		primary.PID, os.Getpid(), backend.PID, initialConfig.Version, configured.Version, freshBackend.PID)
}

func windowHybridProofHidden(t *testing.T, header http.Header) {
	t.Helper()
	for _, name := range []string{sdk.HeaderComponentInstance, sdk.HeaderComponent, sdk.HeaderComponentConfig} {
		if values := header.Values(name); len(values) != 0 {
			t.Fatalf("component proof header escaped to the window: %s=%q", name, values)
		}
	}
}

func windowHybridDecode(t *testing.T, response *httptest.ResponseRecorder, out any) {
	t.Helper()
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), out) != nil {
		t.Fatalf("hybrid window API: HTTP=%d body=%s", response.Code, response.Body)
	}
}

func windowHybridDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("hybrid test refuses non-loopback target %q", address)
	}
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
}

type windowHybridHeaders struct {
	path, cookie string
	err          error
}

type windowHybridConn struct {
	net.Conn
	mu       sync.Mutex
	header   []byte
	seen     bool
	observed chan<- windowHybridHeaders
}

func (connection *windowHybridConn) Write(data []byte) (int, error) {
	size, err := connection.Conn.Write(data)
	connection.mu.Lock()
	var observation *windowHybridHeaders
	if !connection.seen {
		connection.header = append(connection.header, data[:size]...)
		if len(connection.header) > 64<<10 {
			observation = &windowHybridHeaders{err: errors.New("oversized fixture HTTP request headers")}
		} else if end := bytes.Index(connection.header, []byte("\r\n\r\n")); end >= 0 {
			request, parseErr := http.ReadRequest(bufio.NewReader(bytes.NewReader(connection.header[:end+4])))
			observation = &windowHybridHeaders{err: parseErr}
			if parseErr == nil {
				observation.path, observation.cookie = request.URL.Path, request.Header.Get("Cookie")
				request.Body.Close()
			}
		}
		if observation != nil {
			connection.seen, connection.header = true, nil
		}
	}
	connection.mu.Unlock()
	if observation != nil {
		select {
		case connection.observed <- *observation:
		default:
		}
	}
	return size, err
}

func windowHybridCheckHeaders(t *testing.T, ctx context.Context, observed <-chan windowHybridHeaders, path string) {
	t.Helper()
	select {
	case headers := <-observed:
		if headers.err != nil || headers.path != path || headers.cookie != "" {
			t.Fatalf("real helper received the wrong proxy request or the window cookie: %+v", headers)
		}
	case <-ctx.Done():
		t.Fatal("proxy request headers were not observed:", ctx.Err())
	}
}

func windowHybridAwaitClosed(t *testing.T, ctx context.Context, endpoint string) {
	t.Helper()
	location, err := url.Parse(endpoint)
	if err != nil || !windowServiceTarget(location) {
		t.Fatalf("invalid fixture endpoint %q", endpoint)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			t.Fatal("released GUI service generation did not close:", ctx.Err())
		}
		connection, err := windowHybridDial(ctx, "tcp", location.Host)
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return
		}
		if connection != nil {
			connection.Close()
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("released GUI service generation did not close:", ctx.Err())
		}
	}
}
