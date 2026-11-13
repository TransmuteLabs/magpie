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
	"github.com/yetone/magpie/internal/middleware"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/routes"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func installSDKRouting(test *testing.T, manager *extension.Manager, version string, timeout int) {
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

func TestSDKRoutingOrdersOrdinaryProviderAndGroup(test *testing.T) {
	fresh(test)
	test.Setenv("MAGPIE_BUN", filepath.Join(test.TempDir(), "absent"))
	manager := extension.Default()
	test.Cleanup(func() { closeSDKGatewayHook(test, manager) })
	installSDKRouting(test, manager, "1.0.0", 2000)
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
		Keys: []provider.KeyAccount{{Key: "key-b"}, {Key: "key-off", Off: true}}, Routing: "routing.fixture/reverse"}); err != nil {
		test.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		if err := provider.Save(provider.Provider{ID: id, Name: id, Chat: upstream.URL + "/v1", Models: []string{"m"}, Key: id}); err != nil {
			test.Fatal(err)
		}
	}
	if err := provider.SaveGroup(provider.Group{ID: "sdk-pair", Name: "SDK Pair", Members: []string{"first/m", "second/m"},
		Routing: "routing.fixture/reverse"}); err != nil {
		test.Fatal(err)
	}
	if !slices.ContainsFunc(middleware.Routings(), func(item middleware.Routing) bool {
		return item.ID == "routing.fixture/reverse"
	}) || manager.Running("routing.fixture") {
		test.Fatal("metadata discovery did not remain cold")
	}
	server := New()
	ask := func(model, want string) {
		test.Helper()
		forgetRouting()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hello"}]}`))
		request.RemoteAddr = "127.0.0.1:12345"
		server.Front().ServeHTTP(recorder, request)
		if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"content":"from `+want+`"`) {
			test.Fatalf("ordinary %s: %d %s, wanted %s", model, recorder.Code, recorder.Body.String(), want)
		}
	}
	ask("relay/m", "key-b")
	ask("group/sdk-pair", "second")
	lock.Lock()
	if !slices.Equal(tried, []string{"key-b", "second"}) {
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
	if len(tried) != 2 {
		test.Errorf("denied call reached inference: %v", tried)
	}
	lock.Unlock()

	for _, mode := range []string{"invalid", "error"} {
		configureSDKRouting(test, manager, map[string]any{"mode": mode})
		ask("relay/m", "key-a")
	}
	other := extension.NewManager(manager.Namespace())
	defer closeSDKGatewayHook(test, other)
	if err := other.SetEnabled(test.Context(), "routing.fixture", false); err != nil {
		test.Fatal(err)
	}
	if routes.Served("routing.fixture/reverse") {
		test.Fatal("disabled routing remained discoverable")
	}
	ask("relay/m", "key-a")
	if err := other.SetEnabled(test.Context(), "routing.fixture", true); err != nil {
		test.Fatal(err)
	}
	configureSDKRouting(test, other, map[string]any{})
	installSDKRouting(test, other, "2.0.0", 2000)
	ask("relay/m", "key-a")
}

func TestSDKRoutingTimeoutIncludesColdStartupAndFailsOpen(test *testing.T) {
	fresh(test)
	manager := extension.Default()
	test.Cleanup(func() { closeSDKGatewayHook(test, manager) })
	installSDKRouting(test, manager, "1.0.0", 100)
	configureSDKRouting(test, manager, map[string]any{"startupDelay": 30000})
	strategy, found := routes.Find("routing.fixture/reverse")
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
