package gateway

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/routes"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func serveKeepAliveGateway(t *testing.T, server *Server) (context.CancelFunc, <-chan error) {
	t.Helper()
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("gateway did not join idle authentication renewal")
		}
	})
	return cancel, done
}

func TestExtensionKeepAliveFollowsOrdinaryGatewayLifecycle(t *testing.T) {
	t.Run("process catalog", func(t *testing.T) { testExtensionKeepAliveGateway(t, false) })
	t.Run("serving catalog", func(t *testing.T) { testExtensionKeepAliveGateway(t, true) })
}

func testExtensionKeepAliveGateway(t *testing.T, isolated bool) {
	fresh(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	release, dispatched := make(chan struct{}), make(chan sdk.Scope, 1)
	catalog := make(chan string, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/sdk-auth-renew" {
			http.NotFound(writer, request)
			return
		}
		var exchange struct {
			Scope sdk.Scope `json:"scope"`
		}
		if err := json.NewDecoder(request.Body).Decode(&exchange); err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		calls.Add(1)
		select {
		case catalog <- request.URL.Query().Get("catalog"):
			dispatched <- exchange.Scope
		default:
			t.Error("idle gateway refreshed more than once")
		}
		<-release
		_ = json.NewEncoder(writer).Encode(sdk.AuthResult{
			ID: exchange.Scope.Account, Credentials: json.RawMessage(`{"token":"synthetic-renewed"}`),
			ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli(),
		})
	}))
	t.Cleanup(upstream.Close)
	process := extensionhost.Default()
	t.Cleanup(func() { closeSDKGatewayHook(t, process) })
	directory, entry := t.TempDir(), "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	command := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"), "build",
		"-o", filepath.Join(directory, entry), "../provider/testdata/extensionkeepaliveworker")
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build gateway renewal fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{
		Schema: 1, ID: "keepalive.gateway", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8},
			Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.AccountAuth}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Fixture", Protocols: []protocolext.Protocol{"chat"},
			KeepAlive: &sdk.AccountKeepAliveDescriptor{}}},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: manifest.ID, Provider: "vendor", Account: "selected", Generation: 1}
	install := func(manager *extensionhost.Manager, owner string) {
		t.Helper()
		if _, err := manager.Install(t.Context(), directory); err != nil {
			t.Fatal(err)
		}
		auth, _ := json.Marshal(map[string]string{"url": upstream.URL + "/sdk-auth-renew?catalog=" + owner, "token": "synthetic-original"})
		if _, err := manager.Store().SaveAccount(extensionhost.Account{
			Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account, Auth: auth,
			ExpiresAt: time.Now().Add(30 * time.Second).UnixMilli(),
		}, 0); err != nil {
			t.Fatal(err)
		}
	}
	install(process, "process")
	if err := provider.RegisterExtensionLogin(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	if providers, err := provider.ExtensionProviders(); err != nil || len(providers) != 1 {
		t.Fatalf("ordinary gateway account discovery: %v %v", providers, err)
	}
	server := New()
	manager, owner := process, "process"
	if isolated {
		owner = "serving"
		manager = extensionhost.NewManager(t.TempDir())
		t.Cleanup(func() { closeSDKGatewayHook(t, manager) })
		install(manager, owner)
		server = newServer(manager, routes.NewCatalog())
	}
	t.Cleanup(server.idle.stop)
	web := httptest.NewServer(server.Front())
	response, err := web.Client().Get(web.URL + "/v1/models")
	if err != nil {
		web.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	web.Close()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { busy.Close() })
	t.Setenv("MAGPIE_ADDR", busy.Addr().String())
	if err := server.ListenAndServe(t.Context()); err == nil {
		t.Fatal("occupied gateway listener was accepted")
	}
	busy.Close()
	if manager.Running(scope.Plugin) || calls.Load() != 0 {
		t.Fatal("New/Front/failed bind renewed an idle SDK account")
	}
	cancel, done := serveKeepAliveGateway(t, server)
	t.Cleanup(unblock)
	select {
	case actual := <-dispatched:
		if actual.Plugin != scope.Plugin || actual.Account != scope.Account || actual.Generation != scope.Generation {
			t.Fatalf("gateway renewed the wrong account: %+v", actual)
		}
		if actualOwner := <-catalog; actualOwner != owner {
			t.Fatalf("gateway renewed the %s catalog, want %s", actualOwner, owner)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ordinary gateway did not renew the due SDK account")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("gateway shutdown abandoned the rotating-token refresh: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("gateway did not finish after its refresh completed")
	}
	current, err := manager.Store().RefreshedAccount(scope)
	if err != nil || current.Generation != 2 || calls.Load() != 1 {
		t.Fatalf("gateway idle renewal lacked a single proven successor: generation=%d calls=%d err=%v",
			current.Generation, calls.Load(), err)
	}
	if isolated {
		account, err := process.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
		if err != nil || account.Generation != scope.Generation {
			t.Fatalf("idle gateway changed the process catalog: generation=%d err=%v", account.Generation, err)
		}
	}
}

func TestExtensionKeepAliveEmptyGatewayNeedsNoBun(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	t.Setenv("PATH", os.Getenv(standInsEnv))
	t.Setenv("MAGPIE_BUN", "")
	plugin.Restart()
	t.Cleanup(plugin.Settle)
	manager := extensionhost.Default()
	if entries, err := manager.List(); err != nil || len(entries) != 0 {
		t.Fatalf("empty gateway catalog: %v %v", entries, err)
	}
	server := New()
	t.Cleanup(server.idle.stop)
	cancel, done := serveKeepAliveGateway(t, server)
	waitFor(t, "empty gateway listener", func() bool { return server.Addr() != "" })
	client := &http.Client{Timeout: 10 * time.Second}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+server.Addr()+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || plugin.Running() || proc.FindTool("bun") != "" {
		t.Fatalf("empty gateway started or required Bun: status=%d running=%t", response.StatusCode, plugin.Running())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("empty gateway did not finish")
	}
}
