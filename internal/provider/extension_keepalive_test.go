package provider

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

type keepAliveExchange struct {
	Scope sdk.Scope       `json:"scope"`
	Auth  json.RawMessage `json:"auth"`
	Proxy string          `json:"proxy"`
}

type keepAliveFixture struct {
	manager   *extensionhost.Manager
	scope     sdk.Scope
	directory string
	endpoint  string
	calls     atomic.Int32
	exchanges chan keepAliveExchange
}

func keepAliveOwnerFixture(t *testing.T, hold func(http.ResponseWriter, *http.Request)) *keepAliveFixture {
	t.Helper()
	claudeHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	fixture := &keepAliveFixture{exchanges: make(chan keepAliveExchange, 32)}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/sdk-auth-renew" {
			http.NotFound(writer, request)
			return
		}
		var exchange keepAliveExchange
		if err := json.NewDecoder(request.Body).Decode(&exchange); err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		fixture.calls.Add(1)
		fixture.exchanges <- exchange
		if hold != nil {
			hold(writer, request)
		}
		var credentials map[string]string
		_ = json.Unmarshal(exchange.Auth, &credentials)
		credentials["token"] = "synthetic-rotated"
		auth, _ := json.Marshal(credentials)
		_ = json.NewEncoder(writer).Encode(sdk.AuthResult{
			ID: exchange.Scope.Account, Credentials: auth, ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli(),
		})
	}))
	t.Cleanup(server.Close)
	fixture.endpoint = server.URL + "/sdk-auth-renew"
	fixture.manager = extensionhost.Default()
	before := providerExtensionHost
	providerExtensionHost = func() extensionProviderHost { return fixture.manager }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := fixture.manager.Close(ctx); err != nil {
			t.Error(err)
		}
		providerExtensionHost = before
		Changed()
	})
	fixture.directory = t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	build := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"), "build",
		"-o", filepath.Join(fixture.directory, entry), "./testdata/extensionkeepaliveworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build keepalive fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{
		Schema: 1, ID: "keepalive.fixture", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8},
			Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.AccountAuth}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Fixture", Protocols: []protocolext.Protocol{"chat"},
			KeepAlive: &sdk.AccountKeepAliveDescriptor{}}},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(fixture.directory, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.manager.Install(t.Context(), fixture.directory); err != nil {
		t.Fatal(err)
	}
	fixture.scope = sdk.Scope{Plugin: manifest.ID, Provider: "vendor", Account: "selected", Generation: 1}
	auth, _ := json.Marshal(map[string]string{"url": fixture.endpoint, "token": "synthetic-original"})
	if _, err := saveExtensionAuth(t.Context(), fixture.manager, sdk.Scope{Plugin: manifest.ID, Provider: "vendor"}, sdk.AuthResult{
		ID: fixture.scope.Account, Credentials: auth, ExpiresAt: time.Now().Add(30 * time.Second).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if providers, err := ExtensionProviders(); err != nil || len(providers) != 1 {
		t.Fatalf("ordinary account listing: %v %v", providers, err)
	}
	return fixture
}

func TestExtensionKeepAliveUsesTransactionalRefresh(t *testing.T) {
	fixture := keepAliveOwnerFixture(t, nil)
	if fixture.manager.Running(fixture.scope.Plugin) {
		t.Fatal("account listing started an idle worker")
	}
	keeper := extensionSignInKeeper{host: fixture.manager}
	keeper.look(t.Context(), time.Now())
	account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
	if err != nil || account.Generation != 2 || account.ExpiresAt < time.Now().Add(time.Hour).UnixMilli() ||
		fixture.calls.Load() != 1 {
		t.Fatalf("idle SDK sign-in was not renewed by the ordinary transaction: generation=%d calls=%d err=%v",
			account.Generation, fixture.calls.Load(), err)
	}
	proven, err := fixture.manager.Store().RefreshedAccount(fixture.scope)
	if err != nil || proven.Generation != account.Generation {
		t.Fatalf("idle renewal has no exact refresh successor receipt: %+v %v", proven, err)
	}
	exchange := <-fixture.exchanges
	if exchange.Scope.Plugin != fixture.scope.Plugin || exchange.Scope.Account != fixture.scope.Account ||
		exchange.Scope.Generation != 1 || exchange.Scope.Operation == "" {
		t.Fatalf("refresh transaction lost the selected account: %+v", exchange.Scope)
	}
	keeper.look(t.Context(), time.Now())
	receipt, err := fixture.manager.RefreshAccount(t.Context(), fixture.scope.Plugin, fixture.scope)
	if err != nil || len(receipt.Credentials) != 0 || fixture.calls.Load() != 1 {
		t.Fatalf("idle/foreground replay spent the token twice or leaked credentials: calls=%d err=%v", fixture.calls.Load(), err)
	}
}

func TestExtensionKeepAliveRejectsChangedSelectionBeforeDispatch(t *testing.T) {
	for _, change := range []string{"account", "config", "disabled", "re-enabled", "package", "account off", "provider off", "unreadable discovery", "cancel", "network hold"} {
		t.Run(change, func(t *testing.T) {
			fixture := keepAliveOwnerFixture(t, nil)
			selected, err := extensionSignInsKept(t.Context(), fixture.manager)
			if err != nil || len(selected) != 1 {
				t.Fatalf("selected idle account: %d %v", len(selected), err)
			}
			ctx := t.Context()
			switch change {
			case "account":
				account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.manager.Store().SaveAccount(account, account.Generation); err != nil {
					t.Fatal(err)
				}
			case "config":
				if _, err := fixture.manager.CompareAndSwapModuleConfig(ctx, fixture.scope.Plugin, "0", json.RawMessage(`{"changed":true}`)); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := fixture.manager.SetEnabled(ctx, fixture.scope.Plugin, false); err != nil {
					t.Fatal(err)
				}
			case "re-enabled":
				foreign := extensionhost.NewCatalog(fixture.manager.Namespace())
				for _, enabled := range []bool{false, true} {
					if err := foreign.SetEnabled(ctx, fixture.scope.Plugin, enabled); err != nil {
						t.Fatal(err)
					}
				}
			case "package":
				path := filepath.Join(fixture.directory, sdk.ManifestFile)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var manifest sdk.Manifest
				_ = json.Unmarshal(raw, &manifest)
				manifest.Version = "1.0.1"
				raw, _ = json.Marshal(manifest)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.manager.Install(ctx, fixture.directory); err != nil {
					t.Fatal(err)
				}
			case "account off":
				if err := SetLoginOn(ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider), fixture.scope.Account, false); err != nil {
					t.Fatal(err)
				}
			case "provider off":
				raw, _ := json.Marshal(file{Providers: []Provider{{
					ID: ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider), Off: true,
				}}})
				if err := os.WriteFile(Path(), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "unreadable discovery":
				damageExtensionDiscoverySettings(t)
			case "cancel":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			case "network hold":
				ctx = netproxy.Hold(ctx, errors.New("synthetic manual network hold"))
			}
			if err := keepExtensionSignedIn(ctx, selected[0]); err == nil || fixture.calls.Load() != 0 {
				t.Fatalf("revoked %s selection dispatched: calls=%d err=%v", change, fixture.calls.Load(), err)
			}
		})
	}
}

func damageExtensionDiscoverySettings(t *testing.T) {
	t.Helper()
	path := settings.Path()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".bak", []byte(`{"noAccountDiscovery":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"noAccountDiscovery":`), 0600); err != nil {
		t.Fatal(err)
	}
	if recovered, err := settings.LoadChecked(); err != nil || settings.AccountDiscoveryOffIn(recovered) {
		t.Fatalf("fixture did not preserve a usable discovery-enabled backup: %v", err)
	}
}

func TestExtensionKeepAliveJoinsRefreshAndSharesForegroundOwner(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	fixture := keepAliveOwnerFixture(t, func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(unblock)
	ctx, cancel := context.WithCancel(t.Context())
	stop := KeepExtensionsSignedIn(ctx, fixture.manager)
	t.Cleanup(func() { cancel(); unblock(); stop() })
	select {
	case <-fixture.exchanges:
	case <-time.After(5 * time.Second):
		t.Fatal("ordinary idle lifecycle never dispatched the due account")
	}
	foreground := make(chan error, 1)
	foregroundDone := make(chan struct{})
	go func() {
		defer close(foregroundDone)
		_, err := fixture.manager.RefreshAccount(t.Context(), fixture.scope.Plugin, fixture.scope)
		foreground <- err
	}()
	t.Cleanup(func() { cancel(); unblock(); stop(); <-foregroundDone })
	cancel()
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		unblock()
		t.Fatal("idle lifecycle returned before its rotating-token refresh completed")
	case <-time.After(25 * time.Millisecond):
	}
	unblock()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("idle lifecycle did not join its completed renewal")
	}
	if err := <-foreground; err != nil {
		t.Fatal(err)
	}
	current, err := fixture.manager.Store().RefreshedAccount(fixture.scope)
	if err != nil || current.Generation != 2 || fixture.calls.Load() != 1 {
		t.Fatalf("shutdown/foreground did not share the single transaction: generation=%d calls=%d err=%v",
			current.Generation, fixture.calls.Load(), err)
	}
}

func TestExtensionKeepAliveUsesSelectedAccountProxy(t *testing.T) {
	fixture := keepAliveOwnerFixture(t, nil)
	account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
	if err != nil {
		t.Fatal(err)
	}
	account.Auth = json.RawMessage(`{"url":"http://keepalive.vendor.invalid/sdk-auth-renew","token":"synthetic-proxied"}`)
	account, err = fixture.manager.Store().SaveAccount(account, account.Generation)
	if err != nil {
		t.Fatal(err)
	}
	fixture.scope.Generation = account.Generation
	provider := ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider)
	setProxies(t, provider, "http://127.0.0.1:9", map[string]string{fixture.scope.Account: "@keepalive-proxy"})
	set := settings.Load()
	set.Proxies = []settings.NamedProxy{{ID: "keepalive-proxy", Name: "Fixture", URL: fixture.endpoint[:len(fixture.endpoint)-len("/sdk-auth-renew")]}}
	if err := settings.Save(set); err != nil {
		t.Fatal(err)
	}
	keeper := extensionSignInKeeper{host: fixture.manager}
	keeper.look(t.Context(), time.Now())
	current, err := fixture.manager.Store().RefreshedAccount(fixture.scope)
	if err != nil || current.Generation != fixture.scope.Generation+1 || fixture.calls.Load() != 1 {
		t.Fatalf("idle renewal did not use the selected named proxy: generation=%d calls=%d err=%v",
			current.Generation, fixture.calls.Load(), err)
	}
	exchange := <-fixture.exchanges
	if exchange.Proxy != set.Proxies[0].URL || exchange.Scope.Generation != fixture.scope.Generation {
		t.Fatalf("selected proxy/generation did not reach the real worker: %+v", exchange)
	}
}

func TestExtensionKeepAliveRechecksAfterDispatchedRefresh(t *testing.T) {
	for _, change := range []string{"manual account", "config", "disabled"} {
		t.Run(change, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			fixture := keepAliveOwnerFixture(t, func(http.ResponseWriter, *http.Request) { <-release })
			t.Cleanup(unblock)
			selected, err := extensionSignInsKept(t.Context(), fixture.manager)
			if err != nil || len(selected) != 1 {
				t.Fatalf("selected idle account: %d %v", len(selected), err)
			}
			result := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				result <- keepExtensionSignedIn(t.Context(), selected[0])
			}()
			t.Cleanup(func() { unblock(); <-done })
			select {
			case <-fixture.exchanges:
			case <-time.After(5 * time.Second):
				t.Fatal("idle refresh did not reach its held transaction")
			}
			switch change {
			case "manual account":
				account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
				if err != nil {
					t.Fatal(err)
				}
				account.Auth = json.RawMessage(`{"token":"synthetic-manual-replacement"}`)
				if _, err := fixture.manager.Store().SaveAccount(account, account.Generation); err != nil {
					t.Fatal(err)
				}
			case "config":
				if _, err := fixture.manager.CompareAndSwapModuleConfig(t.Context(), fixture.scope.Plugin, "0", json.RawMessage(`{"changed":true}`)); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := fixture.manager.SetEnabled(t.Context(), fixture.scope.Plugin, false); err != nil {
					t.Fatal(err)
				}
			}
			unblock()
			if err := <-result; err == nil {
				t.Fatal("idle refresh acknowledged a replaced selection", change)
			}
			if fixture.calls.Load() != 1 {
				t.Fatal("replacement replayed the dispatched rotating token", fixture.calls.Load())
			}
			if change == "manual account" {
				current, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
				if err != nil || current.Generation != 2 || string(current.Auth) != `{"token":"synthetic-manual-replacement"}` {
					t.Fatalf("idle renewal overwrote a manual account replacement: generation=%d err=%v", current.Generation, err)
				}
				if _, err := fixture.manager.Store().RefreshedAccount(fixture.scope); err == nil {
					t.Fatal("a manual auth rotation became a refresh-successor receipt")
				}
			}
		})
	}
}
