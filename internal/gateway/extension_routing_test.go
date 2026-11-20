package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/routes"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func installSDKRouting(test *testing.T, manager *extension.Manager, version string, timeout int, registry bool) {
	test.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		test.Fatal(err)
	}
	directory := test.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	ctx, cancel := context.WithTimeout(test.Context(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-o",
		filepath.Join(directory, entry), "./internal/extension/testdata/routingworker")
	build.Dir = root
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		test.Fatalf("build SDK routing fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: "routing.fixture", Version: version, Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 4}, Capabilities: []sdk.Capability{sdk.ModuleRPC}},
		Actions:  []sdk.ActionDescriptor{{ID: "rank", Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		RoutingStrategies: []sdk.RoutingStrategyDescriptor{{ID: "reverse", Name: "Reverse fixture", Action: "rank", TimeoutMs: timeout,
			Flags: sdk.RoutingFlags{Exact: true}}},
	}
	if registry {
		manifest.Contract.Version.Minor = 16
		manifest.Contributions = []sdk.ContributionDescriptor{{
			Point: sdk.ContributionRouting, Version: 1, Routing: &manifest.RoutingStrategies[0],
		}}
		manifest.RoutingStrategies = nil
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		test.Fatal(err)
	}
	if _, err := manager.Install(ctx, directory); err != nil {
		test.Fatal(err)
	}
}

func configureSDKRouting(test *testing.T, manager *extension.Manager, options map[string]any) {
	test.Helper()
	current, err := manager.ModuleConfig(test.Context(), "routing.fixture")
	if err != nil {
		test.Fatal(err)
	}
	raw, _ := json.Marshal(options)
	if _, err := manager.CompareAndSwapModuleConfig(test.Context(), "routing.fixture", current.Version, raw); err != nil {
		test.Fatal(err)
	}
}

func sdkRoutingRequest(ctx context.Context, server *Server, model string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hello"}]}`))
	request.RemoteAddr = "127.0.0.1:12345"
	server.Front().ServeHTTP(recorder, request)
	return recorder
}

func expectSDKRoutingReply(test *testing.T, recorder *httptest.ResponseRecorder, want string) {
	test.Helper()
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"content":"from `+want+`"`) {
		test.Fatalf("ordinary inference: %d %s, wanted %s", recorder.Code, recorder.Body.String(), want)
	}
}

func TestExplicitProtocolGatewayDoesNotUseDefaultRouting(test *testing.T) {
	fresh(test)
	manager := extension.Default()
	test.Cleanup(func() { closeSDKGatewayHook(test, manager) })
	installSDKRouting(test, manager, "1.0.0", 2000, true)
	upstream := serveOn(test, "relay", "key-a", []string{"m"}, &keyed{}, "key-b")
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Chat: upstream.URL + "/v1",
		Models: []string{"m"}, Key: "key-a", Keys: []provider.KeyAccount{{Key: "key-b"}}, Routing: "routing.fixture/reverse"}); err != nil {
		test.Fatal(err)
	}
	expectSDKRoutingReply(test, sdkRoutingRequest(test.Context(), New(), "relay/m"), "key-b")
	expectSDKRoutingReply(test, sdkRoutingRequest(test.Context(), protocolServer(test), "relay/m"), "key-a")
}

func TestSDKRoutingOrdersOrdinaryProviderAndGroup(test *testing.T) {
	test.Run("legacy", func(test *testing.T) { sdkRoutingOrdersOrdinaryProviderAndGroup(test, false) })
	test.Run("registry", func(test *testing.T) { sdkRoutingOrdersOrdinaryProviderAndGroup(test, true) })
}

func sdkRoutingOrdersOrdinaryProviderAndGroup(test *testing.T, registry bool) {
	fresh(test)
	test.Setenv("MAGPIE_BUN", filepath.Join(test.TempDir(), "absent"))
	const routing = "routing.fixture/reverse"
	first := extension.NewManager(test.TempDir())
	second := extension.NewManager(test.TempDir())
	trace := filepath.Join(test.TempDir(), "rank.jsonl")
	for index, manager := range []*extension.Manager{first, second} {
		test.Cleanup(func() { closeSDKGatewayHook(test, manager) })
		installSDKRouting(test, manager, []string{"1.0.0", "2.0.0"}[index], 2000, registry)
	}
	configureSDKRouting(test, second, map[string]any{"trace": trace})
	server := newServer(first, routes.NewCatalog())
	listed := newServer(second, routes.NewCatalog())
	hasRouting := func(server *Server) bool {
		return slices.ContainsFunc(server.RoutingOptions(), func(option routes.Option) bool {
			return option.ID == routing && option.Name == "Reverse fixture"
		})
	}
	if !hasRouting(server) || !hasRouting(listed) || first.Running("routing.fixture") || second.Running("routing.fixture") {
		test.Fatal("listing each gateway's catalog did not remain cold")
	}
	var lock sync.Mutex
	var tried []string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			writer.Header().Set("Content-Type", "application/json")
			io.WriteString(writer, `{"object":"list","data":[{"id":"m","object":"model"}]}`)
			return
		}
		body, _ := io.ReadAll(request.Body)
		if request.Method != http.MethodPost || request.URL.Path != "/v1/chat/completions" || modelOf(body) != "m" {
			http.Error(writer, "wrong inference request", 400)
			return
		}
		key := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		lock.Lock()
		tried = append(tried, key)
		lock.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		io.WriteString(writer, `{"id":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"from `+key+`"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Chat: upstream.URL + "/v1", Models: []string{"m"}, Key: "key-a",
		Keys: []provider.KeyAccount{{Key: "key-b"}, {Key: "key-off", Off: true}}, Routing: routing}); err != nil {
		test.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		if err := provider.Save(provider.Provider{ID: id, Name: id, Chat: upstream.URL + "/v1", Models: []string{"m"}, Key: id}); err != nil {
			test.Fatal(err)
		}
	}
	if err := provider.SaveGroup(provider.Group{ID: "sdk-pair", Name: "SDK Pair", Members: []string{"first/m", "second/m"},
		Routing: routing}); err != nil {
		test.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{ID: "sdk-nested", Name: "SDK Nested", Members: []string{"group/sdk-pair"},
		Routing: provider.Ordered}); err != nil {
		test.Fatal(err)
	}
	ask := func(server *Server, model, want string) {
		test.Helper()
		forgetRouting()
		expectSDKRoutingReply(test, sdkRoutingRequest(test.Context(), server, model), want)
	}
	ask(server, "relay/m", "key-b")
	ask(server, "group/sdk-pair", "second")
	ask(server, "group/sdk-nested", "second")
	ask(listed, "relay/m", "key-a")
	ask(listed, "group/sdk-nested", "first")
	checkTrace := func(minimum int) {
		test.Helper()
		raw, err := os.ReadFile(trace)
		if err != nil {
			test.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) < minimum {
			test.Fatalf("second gateway did not order its pools: %s", raw)
		}
		for _, line := range lines {
			var event struct{ Version string }
			if err := json.Unmarshal([]byte(line), &event); err != nil || event.Version != "2.0.0" {
				test.Fatalf("second gateway invoked a different worker: %q (%v)", line, err)
			}
		}
	}
	checkTrace(2)
	wantAttempts := []string{"key-b", "second", "second", "key-a", "first"}
	lock.Lock()
	if !slices.Equal(tried, wantAttempts) {
		test.Errorf("strategy added or skipped inference attempts: %v", tried)
	}
	lock.Unlock()

	keys, secrets := newCaller(test, "Only first")
	if _, err := access.Update("models-key", access.Change{Key: keys[0].ID, Models: []string{"first/m"}}); err != nil {
		test.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"relay/m","messages":[{"role":"user","content":"denied"}]}`))
	request.Header.Set("Authorization", "Bearer "+secrets[0])
	recorder := httptest.NewRecorder()
	server.Front().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		test.Fatalf("strategy bypassed gateway model hold: %d %s", recorder.Code, recorder.Body.String())
	}
	lock.Lock()
	if len(tried) != len(wantAttempts) {
		test.Errorf("denied call reached inference: %v", tried)
	}
	lock.Unlock()

	for _, mode := range []string{"invalid", "error"} {
		configureSDKRouting(test, first, map[string]any{"mode": mode})
		ask(server, "relay/m", "key-a")
	}
	directory := test.TempDir()
	started, release := filepath.Join(directory, "started"), filepath.Join(directory, "release")
	configureSDKRouting(test, first, map[string]any{"mode": "hold", "started": started, "release": release})
	forgetRouting()
	finished := make(chan *httptest.ResponseRecorder, 1)
	requestCtx, cancel := context.WithCancel(test.Context())
	go func() {
		defer close(finished)
		finished <- sdkRoutingRequest(requestCtx, server, "relay/m")
	}()
	defer func() { cancel(); <-finished }()
	waitFor(test, "SDK routing reached rank", func() bool {
		raw, _ := os.ReadFile(started)
		return string(raw) == "ordering"
	})
	editor := extension.NewCatalog(first.Namespace())
	if err := editor.SetEnabled(test.Context(), "routing.fixture", false); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		test.Fatal(err)
	}
	expectSDKRoutingReply(test, <-finished, "key-a")
	if hasRouting(server) || !hasRouting(listed) {
		test.Fatal("disabling one gateway's rule changed the wrong catalog")
	}
	ask(server, "relay/m", "key-a")
	if err := editor.SetEnabled(test.Context(), "routing.fixture", true); err != nil {
		test.Fatal(err)
	}
	configureSDKRouting(test, first, map[string]any{})
	ask(server, "relay/m", "key-b")
	installSDKRouting(test, first, "2.0.0", 2000, registry)
	ask(server, "relay/m", "key-a")
	closeSDKGatewayHook(test, first)
	ask(listed, "group/sdk-nested", "first")
	checkTrace(3)
}

func TestSDKRoutingSurvivesListenerRestart(test *testing.T) {
	fresh(test)
	manager := extension.Default()
	test.Cleanup(func() { closeSDKGatewayHook(test, manager) })
	installSDKRouting(test, manager, "1.0.0", 2000, true)
	upstream := serveOn(test, "relay", "key-a", []string{"m"}, &keyed{}, "key-b")
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Chat: upstream.URL + "/v1", Models: []string{"m"},
		Key: "key-a", Keys: []provider.KeyAccount{{Key: "key-b"}}, Routing: "routing.fixture/reverse"}); err != nil {
		test.Fatal(err)
	}
	server := New()
	cancel, done := serveKeepAliveGateway(test, server)
	waitFor(test, "gateway listener", func() bool { return server.Addr() != "" })
	expectSDKRoutingReply(test, sdkRoutingRequest(test.Context(), server, "relay/m"), "key-b")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			test.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		test.Fatal("gateway did not stop")
	}
	replacement := New()
	if replacement.extensions != manager || !manager.Running("routing.fixture") {
		test.Fatal("stopping the listener closed the application's routing manager")
	}
	expectSDKRoutingReply(test, sdkRoutingRequest(test.Context(), replacement, "relay/m"), "key-b")
}

func TestSDKRoutingTimeoutIncludesColdStartupAndFailsOpen(test *testing.T) {
	fresh(test)
	manager := extension.Default()
	test.Cleanup(func() { closeSDKGatewayHook(test, manager) })
	installSDKRouting(test, manager, "1.0.0", 100, false)
	configureSDKRouting(test, manager, map[string]any{"startupDelay": 30000})
	strategy, found := New().routingCatalog().Find("routing.fixture/reverse")
	if !found {
		test.Fatal("routing descriptor unavailable")
	}
	seats := []routes.Seat{{Key: "a", Weight: 1}, {Key: "b", Weight: 1}}
	if result := routes.Apply(strategy, "pool", seats); !slices.Equal(result, []int{0, 1}) {
		test.Fatalf("cold startup timeout did not fail open: %v", result)
	}
	configureSDKRouting(test, manager, map[string]any{"mode": "timeout"})
	if result := routes.Apply(strategy, "pool", seats); !slices.Equal(result, []int{0, 1}) {
		test.Fatalf("ordering timeout did not fail open: %v", result)
	}
}
