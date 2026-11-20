package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gwtool"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const refreshToolSpec = "extension:tool.refresh"

var refreshToolArgs = json.RawMessage(`{"value":"hello","_magpie":{"provider":"vendor","account":"selected"}}`)

type toolRefreshFixture struct {
	manager   *extension.Manager
	scope     sdk.Scope
	refreshes atomic.Int32
	toolCalls atomic.Int32
	tool      mcpTool
	server    *httptest.Server
}

func installToolRefreshFixture(test *testing.T, beforeReply func(*toolRefreshFixture)) *toolRefreshFixture {
	test.Helper()
	fresh(test)
	test.Setenv("MAGPIE_NO_ACCOUNTS", "")
	fixture := &toolRefreshFixture{manager: extension.Default(), scope: sdk.Scope{Plugin: "tool.refresh", Provider: "vendor", Account: "selected", Generation: 1}}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || (request.URL.Path != "/" && request.URL.Path != "/tool") {
			http.NotFound(writer, request)
			return
		}
		if request.URL.Path == "/tool" {
			fixture.toolCalls.Add(1)
			writer.WriteHeader(http.StatusOK)
			return
		}
		fixture.refreshes.Add(1)
		var call sdk.Call
		if err := json.NewDecoder(request.Body).Decode(&call); err != nil || call.Scope.Generation != 1 || !strings.Contains(string(call.Auth), "synthetic-original") {
			test.Errorf("refresh did not use original Frame.Auth: %+v %v", call.Scope, err)
		}
		if beforeReply != nil {
			beforeReply(fixture)
		}
		auth, _ := json.Marshal(map[string]string{"url": fixture.server.URL, "token": "synthetic-rotated"})
		_ = json.NewEncoder(writer).Encode(sdk.AuthResult{ID: "selected", Credentials: auth, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	}))
	test.Cleanup(fixture.server.Close)
	directory := test.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	build := proc.CommandContext(testenv.DeadlineContext(test, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", filepath.Join(directory, entry), "./testdata/extensiontoolrefreshworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		test.Fatalf("build fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: fixture.scope.Plugin, Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract:  sdk.Contract{Version: sdk.FullContract().Version, Capabilities: []sdk.Capability{sdk.AccountAuth, sdk.ToolProvider}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Fixture"}},
		Tools:     []sdk.ToolDescriptor{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.manager.Install(test.Context(), directory); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := fixture.manager.Close(ctx); err != nil {
			test.Error(err)
		}
		provider.Changed()
	})
	auth, _ := json.Marshal(map[string]string{"url": fixture.server.URL, "token": "synthetic-original"})
	if _, err := fixture.manager.Store().SaveAccount(extension.Account{Plugin: fixture.scope.Plugin, Provider: fixture.scope.Provider, ID: fixture.scope.Account,
		Auth: auth, ExpiresAt: time.Now().Add(-time.Minute).UnixMilli()}, 0); err != nil {
		test.Fatal(err)
	}
	if _, err := provider.ExtensionProviders(); err != nil {
		test.Fatal(err)
	}
	groups := []plugin.PluginTools{{Spec: refreshToolSpec, Tools: []plugin.Tool{{Name: "echo"}}}}
	fakePluginTools(test, groups)
	runPluginTool = plugin.RunTool
	fixture.tool = namedTools(groups)[0]
	test.Cleanup(gwtool.Serve(gwtool.PluginTools))
	return fixture
}

func (fixture *toolRefreshFixture) run(ctx context.Context, args json.RawMessage) (map[string]any, error) {
	request := httptest.NewRequest(http.MethodPost, ToolsMCPPath, nil).WithContext(ctx)
	request.RemoteAddr = "127.0.0.1:12345"
	return New().toolsMCPRun(request, fixture.tool, args)
}

func checkRefreshedTool(test *testing.T, output any, err error) {
	test.Helper()
	if err != nil {
		test.Errorf("selected tool failed: %v", err)
		return
	}
	body, direct := output.(string)
	if !direct {
		raw, err := json.Marshal(output)
		if err != nil {
			test.Error(err)
			return
		}
		var result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &result); err != nil || len(result.Content) != 1 {
			test.Errorf("invalid MCP tool result: %s, %v", raw, err)
			return
		}
		body = result.Content[0].Text
	}
	var frame struct {
		Scope sdk.Scope `json:"scope"`
		Auth  struct {
			Token string `json:"token"`
		} `json:"auth"`
		Params sdk.ToolRequest `json:"params"`
	}
	if err := json.Unmarshal([]byte(body), &frame); err != nil {
		test.Errorf("invalid worker frame observation: %s, %v", body, err)
		return
	}
	if frame.Scope.Plugin != "tool.refresh" || frame.Scope.Provider != "vendor" || frame.Scope.Account != "selected" ||
		frame.Scope.Generation != 2 || frame.Auth.Token != "synthetic-rotated" || frame.Params.ID != "echo" ||
		string(frame.Params.Arguments) != `{"value":"hello"}` {
		test.Errorf("tool did not receive refreshed Frame.Auth and sanitized args: %s", body)
	}
}

func TestExtensionToolExpiryPreflightOrdinaryPaths(test *testing.T) {
	for _, route := range []string{"MCP", "Library text", "Library result"} {
		test.Run(route, func(test *testing.T) {
			fixture := installToolRefreshFixture(test, nil)
			var output any
			var err error
			switch route {
			case "MCP":
				output, err = fixture.run(test.Context(), refreshToolArgs)
			case "Library text":
				output, err = RunPluginToolOf(test.Context(), refreshToolSpec, "echo", refreshToolArgs, "fixture")
			case "Library result":
				output, err = RunPluginToolResultOf(test.Context(), refreshToolSpec, "echo", refreshToolArgs, "fixture")
			}
			checkRefreshedTool(test, output, err)
			if fixture.refreshes.Load() != 1 {
				test.Errorf("refresh exchanges = %d; want 1", fixture.refreshes.Load())
			}
		})
	}
}

func TestExtensionToolExpiryPreflightConcurrent(test *testing.T) {
	fixture := installToolRefreshFixture(test, nil)
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() {
			output, err := fixture.run(test.Context(), refreshToolArgs)
			checkRefreshedTool(test, output, err)
		})
	}
	wait.Wait()
	if fixture.refreshes.Load() != 1 {
		test.Fatalf("rotating exchanges = %d; want 1", fixture.refreshes.Load())
	}
}

func TestExtensionToolExpiryPreflightControls(test *testing.T) {
	for _, mode := range []string{"unbound", "unknown expiry", "fresh", "network hold", "account off", "disabled", "restricted caller", "invalid args", "canceled", "fresh request error"} {
		test.Run(mode, func(test *testing.T) {
			fixture := installToolRefreshFixture(test, nil)
			ctx, args := test.Context(), refreshToolArgs
			rejected := false
			switch mode {
			case "unbound":
				args = json.RawMessage(`{"value":"public"}`)
			case "unknown expiry", "fresh", "fresh request error":
				account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
				if err != nil {
					test.Fatal(err)
				}
				account.ExpiresAt = 0
				if mode != "unknown expiry" {
					account.ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
				}
				if _, err := fixture.manager.Store().SaveAccount(account, account.Generation); err != nil {
					test.Fatal(err)
				}
				if mode == "fresh request error" {
					args = json.RawMessage(`{"value":"fixture-401","_magpie":{"provider":"vendor","account":"selected"}}`)
					rejected = true
				}
			case "restricted caller":
				ctx, rejected = access.WithIdentity(ctx, access.Identity{ReadOnly: true}), true
			case "invalid args":
				args, rejected = json.RawMessage(`[]`), true
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				rejected = true
			case "network hold":
				ctx, rejected = netproxy.Hold(ctx, errors.New("fixture hold")), true
			case "account off":
				if err := provider.SetLoginOn(provider.ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider), fixture.scope.Account, false); err != nil {
					test.Fatal(err)
				}
				rejected = true
			case "disabled":
				if err := fixture.manager.SetEnabled(ctx, fixture.scope.Plugin, false); err != nil {
					test.Fatal(err)
				}
				rejected = true
			}
			output, err := fixture.run(ctx, args)
			if rejected && mode != "fresh request error" && fixture.toolCalls.Load() != 0 {
				test.Error("rejected control reached tool callback")
			}
			if mode == "fresh request error" && fixture.toolCalls.Load() != 1 {
				test.Error("request error was replayed or never dispatched")
			}
			if (err != nil) != rejected {
				test.Errorf("control output=%v error=%v rejected=%v", output, err, rejected)
			}
			if fixture.refreshes.Load() != 0 {
				test.Errorf("control refreshed: %d", fixture.refreshes.Load())
			}
		})
	}
}

func TestExtensionToolExpiryPreflightRevocationDuringRefresh(test *testing.T) {
	for _, mode := range []string{"manual replacement", "config", "account off", "disabled"} {
		test.Run(mode, func(test *testing.T) {
			fixture := installToolRefreshFixture(test, func(fixture *toolRefreshFixture) {
				switch mode {
				case "manual replacement":
					_, err := fixture.manager.Store().SaveAccount(extension.Account{Plugin: fixture.scope.Plugin, Provider: fixture.scope.Provider, ID: fixture.scope.Account,
						Auth: json.RawMessage(`{"token":"manual"}`), ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, 1)
					if err != nil {
						test.Error(err)
					}
				case "config":
					if _, err := fixture.manager.CompareAndSwapModuleConfig(test.Context(), fixture.scope.Plugin, "0", json.RawMessage(`{"changed":true}`)); err != nil {
						test.Error(err)
					}
				case "disabled":
					if err := extension.NewCatalog(fixture.manager.Namespace()).SetEnabled(test.Context(), fixture.scope.Plugin, false); err != nil {
						test.Error(err)
					}
				case "account off":
					if err := provider.SetLoginOn(provider.ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider), fixture.scope.Account, false); err != nil {
						test.Error(err)
					}
				}
			})
			output, err := fixture.run(test.Context(), refreshToolArgs)
			if err == nil {
				test.Errorf("revoked selection dispatched tool: %v", output)
			}
			if fixture.refreshes.Load() != 1 {
				test.Errorf("refresh exchange count = %d", fixture.refreshes.Load())
			}
			if fixture.toolCalls.Load() != 0 {
				test.Errorf("revoked selection reached tool callback")
			}
			if mode == "manual replacement" {
				account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
				if err != nil || account.Generation != 2 || string(account.Auth) != `{"token":"manual"}` {
					test.Errorf("refresh overwrote manual replacement: generation=%d, error=%v", account.Generation, err)
				}
			}
			if mode == "disabled" {
				if _, err := fixture.manager.Manifest(fixture.scope.Plugin); err == nil {
					test.Error("refresh re-enabled the disabled module")
				}
			}
			if mode == "account off" {
				before := fixture.refreshes.Load()
				if _, err := fixture.run(test.Context(), refreshToolArgs); err == nil {
					test.Error("refresh re-enabled the disabled account")
				}
				if fixture.refreshes.Load() != before {
					test.Error("disabled account triggered another refresh")
				}
			}

		})
	}
}

func TestExtensionToolExpiryPreflightRevocationBeforeDispatch(test *testing.T) {
	for _, mode := range []string{"manual replacement", "config", "disabled", "hold"} {
		test.Run(mode, func(test *testing.T) {
			fixture := installToolRefreshFixture(test, nil)
			ctx, args, err := extensionToolContext(test.Context(), refreshToolSpec, refreshToolArgs)
			if err != nil {
				test.Fatal(err)
			}
			switch mode {
			case "manual replacement":
				account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
				if err != nil {
					test.Fatal(err)
				}
				account.Auth = json.RawMessage(`{"token":"manual"}`)
				if _, err := fixture.manager.Store().SaveAccount(account, account.Generation); err != nil {
					test.Fatal(err)
				}
			case "config":
				if _, err := fixture.manager.CompareAndSwapModuleConfig(ctx, fixture.scope.Plugin, "0", json.RawMessage(`{"changed":true}`)); err != nil {
					test.Fatal(err)
				}
			case "disabled":
				if err := fixture.manager.SetEnabled(ctx, fixture.scope.Plugin, false); err != nil {
					test.Fatal(err)
				}
			case "hold":
				ctx = netproxy.Hold(ctx, errors.New("fixture late hold"))
			}
			if output, err := plugin.RunExtensionTool(ctx, refreshToolSpec, "echo", args); err == nil {
				test.Errorf("late revoked tool dispatched: %s", output.Content)
			}
			if fixture.toolCalls.Load() != 0 {
				test.Error("late revoked selection reached tool callback")
			}
		})
	}
}
