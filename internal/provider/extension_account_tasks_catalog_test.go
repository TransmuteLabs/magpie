package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

const accountTaskCatalogWorker = `package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func main() {
	raw, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		panic(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		panic(err)
	}
	module := sdk.Module{Manifest: manifest, Handlers: map[string]sdk.Handler{},
		Actions: map[string]sdk.Handler{"paid.ping": func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
			return nil, errors.New("admission must not dispatch paid work")
		}}}
	for _, method := range []string{sdk.MethodAuthStart, sdk.MethodAuthFinish, sdk.MethodAuthRefresh, sdk.MethodAuthLogout} {
		module.Handlers[method] = func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			return sdk.AuthResult{ID: call.Scope.Account, Credentials: call.Auth}, nil
		}
	}
	module.Handlers[sdk.MethodProviderExecute] = func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
		return nil, errors.New("admission must not start inference")
	}
	module.Handlers[sdk.MethodProviderUsage] = func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
		var request sdk.UsageRequest
		if err := json.Unmarshal(call.Params, &request); err != nil {
			return nil, err
		}
		return sdk.UsageResult{Revision: request.Revision, Windows: []sdk.UsageWindow{{Name: "quota", Used: 10}}}, nil
	}
	module.Handlers[sdk.MethodProviderModels] = func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
		var auth struct { URL string }
		if err := json.Unmarshal(call.Auth, &auth); err != nil {
			return nil, err
		}
		var proxy sdk.NetworkProxyResult
		if err := stream.HostCall(ctx, sdk.MethodNetworkProxy, sdk.NetworkProxyRequest{URL: auth.URL}, &proxy); err != nil {
			return nil, err
		}
		moduleContext, _ := sdk.ModuleContextFrom(ctx)
		raw, err := json.Marshal(struct {
			Scope sdk.Scope
			Config string
		}{call.Scope, moduleContext.Config.Version})
		if err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, auth.URL, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		raw, err = io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK {
			return nil, errors.New("fixture catalog read failed")
		}
		return json.RawMessage(raw), nil
	}
	if sdk.Serve(context.Background(), os.Stdin, os.Stdout, module) != nil {
		os.Exit(1)
	}
}
`

type accountTaskCatalogFixture struct {
	manager  *extensionhost.Manager
	entry    extensionhost.Installed
	source   string
	calls    atomic.Int32
	reply    atomic.Value
	received chan string
}

func newAccountTaskCatalogFixture(t *testing.T) *accountTaskCatalogFixture {
	t.Helper()
	claudeHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	fixture := &accountTaskCatalogFixture{manager: extensionhost.Default(), received: make(chan string, 16)}
	fixture.reply.Store(func(writer http.ResponseWriter) {
		_, _ = io.WriteString(writer, `{"models":[{"id":"m1","protocols":["chat"]}]}`)
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/models" {
			http.NotFound(writer, request)
			return
		}
		var exchange struct {
			Scope  sdk.Scope
			Config string
		}
		if json.NewDecoder(request.Body).Decode(&exchange) != nil ||
			exchange.Scope.Plugin != "tasks.catalog" || exchange.Scope.Provider != "vendor" ||
			exchange.Scope.Account != "selected" || exchange.Scope.Generation != 1 ||
			exchange.Scope.Operation != "" || exchange.Scope.Session != "" {
			t.Error("model acquisition lost its selected account scope")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		fixture.calls.Add(1)
		fixture.received <- exchange.Config
		fixture.reply.Load().(func(http.ResponseWriter))(writer)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := fixture.manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	fixture.source = t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	source := filepath.Join(t.TempDir(), "worker.go")
	if err := os.WriteFile(source, []byte(accountTaskCatalogWorker), 0600); err != nil {
		t.Fatal(err)
	}
	build := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"), "build",
		"-mod=readonly", "-o", filepath.Join(fixture.source, entry), source)
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build account task catalog fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: "tasks.catalog", Version: "1.0.0",
		Runtime: "exec", Entry: entry, Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 9},
			Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.AccountAuth, sdk.ModelDiscovery, sdk.UsageReporter, sdk.ModuleRPC}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Task catalog fixture",
			Protocols: []protocolext.Protocol{"chat"}, AccountTasks: []sdk.AccountTaskDescriptor{{ID: "ping", Action: "paid.ping"}}}},
		Actions: []sdk.ActionDescriptor{{ID: "paid.ping", Effect: "write", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.source, sdk.ManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	fixture.entry, err = fixture.manager.Install(t.Context(), fixture.source)
	if err != nil {
		t.Fatal(err)
	}
	auth, _ := json.Marshal(map[string]string{"url": upstream.URL + "/models", "token": "synthetic"})
	if _, err := fixture.manager.Store().SaveAccount(extensionhost.Account{
		Plugin: manifest.ID, Provider: "vendor", ID: "selected", Auth: auth}, 0); err != nil {
		t.Fatal(err)
	}
	if all, err := ExtensionProviders(); err != nil || len(all) != 1 {
		t.Fatal("ordinary account discovery failed", all, err)
	}
	return fixture
}

func (fixture *accountTaskCatalogFixture) selection(t *testing.T) extensionhost.AccountTaskSelection {
	t.Helper()
	config, err := fixture.manager.ModuleConfig(t.Context(), fixture.entry.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	return extensionhost.AccountTaskSelection{
		Scope:    sdk.Scope{Plugin: fixture.entry.Manifest.ID, Provider: "vendor", Account: "selected", Generation: 1},
		Revision: fixture.entry.Revision, Config: config.Version, Model: "m1"}
}

func (fixture *accountTaskCatalogFixture) changeConfig(ctx context.Context) error {
	foreign := extensionhost.NewManager(fixture.manager.Namespace())
	defer foreign.Close(context.Background())
	config, err := foreign.ModuleConfig(ctx, fixture.entry.Manifest.ID)
	if err != nil {
		return err
	}
	_, err = foreign.CompareAndSwapModuleConfig(ctx, fixture.entry.Manifest.ID,
		config.Version, json.RawMessage(`{"enabled":true}`))
	return err
}

func TestExtensionAccountTaskFreshUsageKeepsPolicyAuthority(t *testing.T) {
	for _, initiallyRefused := range []bool{false, true} {
		t.Run(fmt.Sprint(initiallyRefused), func(t *testing.T) {
			fixture := newAccountTaskCatalogFixture(t)
			selection := fixture.selection(t)
			id := ExtensionProviderID(selection.Scope.Plugin, selection.Scope.Provider)
			logins, _ := extensionLogins(id)
			if initiallyRefused {
				token, err := logins[0].extension.beginUsageAuth(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if err := logins[0].extension.finishUsageAuth(t.Context(), token, true); err != nil {
					t.Fatal(err)
				}
			}
			_, _, _, check, err := PrepareExtensionAccountTask(t.Context(), fixture.manager, selection)
			if err != nil {
				t.Fatal("fresh usage did not admit the selected task", err)
			}
			if err := check(t.Context()); err != nil {
				t.Fatal("admission's own usage read revoked paid task authority", err)
			}
			StaleAllowance(id, selection.Scope.Account)
			if quota := LoginUsage(t.Context(), id)[selection.Scope.Account]; quota.Error != "" || len(quota.Windows) != 1 {
				t.Fatalf("ordinary usage refresh failed: %+v", quota)
			}
			if err := check(t.Context()); err != nil {
				t.Fatal("ordinary fresh usage revoked paid task authority", err)
			}
			logins, _ = extensionLogins(id)
			token, err := logins[0].extension.beginUsageAuth(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := logins[0].extension.finishUsageAuth(t.Context(), token, true); err != nil {
				t.Fatal(err)
			}
			if err := check(t.Context()); err == nil {
				t.Fatal("task retained authority after a selected-account authentication refusal")
			}
		})
	}
}

func TestExtensionAccountTaskRefreshesHotConfigCatalog(t *testing.T) {
	fixture := newAccountTaskCatalogFixture(t)
	before := extensionAccounts()[0].Account.extension
	if _, err := before.fetchModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-fixture.received
	if err := fixture.changeConfig(t.Context()); err != nil {
		t.Fatal(err)
	}
	selection := fixture.selection(t)
	setProxies(t, ExtensionProviderID(selection.Scope.Plugin, selection.Scope.Provider),
		"http://127.0.0.1:18080", map[string]string{"selected": "direct"})
	if models, err := extensionAccounts()[0].Account.extension.models(); err != nil || len(models) != 0 {
		t.Fatal("hot configuration reused an older catalog", models, err)
	}
	ctx, selected, scope, check, err := PrepareExtensionAccountTask(t.Context(), fixture.manager, selection)
	if err != nil {
		t.Fatal("current paid admission did not acquire its hot configuration catalog", err)
	}
	if fixture.calls.Load() != 2 || <-fixture.received != selection.Config ||
		scope.Generation != 1 || selected.Account.User != "selected" || netproxy.Choice(ctx) != "direct" {
		t.Fatal("paid catalog acquisition lost current configuration/account/proxy")
	}
	if err := check(t.Context()); err != nil {
		t.Fatal("unchanged admission lost authority", err)
	}
}

func TestExtensionAccountTaskCatalogAcquisitionFailsClosed(t *testing.T) {
	for _, scenario := range []string{"empty", "removed", "malformed", "invalid-json", "failed", "configuration", "account", "package", "disabled", "hold", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newAccountTaskCatalogFixture(t)
			pinned := extensionAccounts()[0].Account.extension
			if _, err := pinned.fetchModels(t.Context()); err != nil {
				t.Fatal(err)
			}
			<-fixture.received
			selection := fixture.selection(t)
			catalog, err := fixture.manager.Store().StateGet(sdk.Scope{Plugin: selection.Scope.Plugin}, pinned.modelCatalogKey())
			if err != nil {
				t.Fatal(err)
			}
			fixture.reply.Store(func(writer http.ResponseWriter) {
				switch scenario {
				case "empty":
					_, _ = io.WriteString(writer, `{"models":[]}`)
				case "removed":
					_, _ = io.WriteString(writer, `{"models":[{"id":"other"}]}`)
				case "malformed":
					_, _ = io.WriteString(writer, `{"models":[{"id":"m1","maxOutput":-1}]}`)
				case "invalid-json":
					_, _ = io.WriteString(writer, `{"models":`)
				case "failed":
					writer.WriteHeader(http.StatusServiceUnavailable)
				case "configuration":
					if err := fixture.changeConfig(t.Context()); err != nil {
						t.Error(err)
					}
				case "account":
					account, err := fixture.manager.Store().GetAccount(selection.Scope.Plugin, selection.Scope.Provider, selection.Scope.Account)
					if err == nil {
						account.Auth = json.RawMessage(`{"token":"replacement"}`)
						_, err = fixture.manager.Store().SaveAccount(account, account.Generation)
					}
					if err != nil {
						t.Error(err)
					}
				case "package":
					manifest := fixture.entry.Manifest.Clone()
					manifest.Version = "1.0.1"
					raw, err := json.Marshal(manifest)
					if err == nil {
						err = os.WriteFile(filepath.Join(fixture.source, sdk.ManifestFile), raw, 0600)
					}
					if err == nil {
						_, err = extensionhost.NewCatalog(fixture.manager.Namespace()).Install(t.Context(), fixture.source,
							func(ctx context.Context, _ extensionhost.Installed) error { return ctx.Err() })
					}
					if err != nil {
						t.Error(err)
					}
				case "disabled":
					if err := extensionhost.NewCatalog(fixture.manager.Namespace()).SetEnabled(t.Context(), selection.Scope.Plugin, false); err != nil {
						t.Error(err)
					}
				}
				if scenario == "configuration" || scenario == "account" || scenario == "package" || scenario == "disabled" {
					_, _ = io.WriteString(writer, `{"models":[{"id":"m1"},{"id":"late"}]}`)
				}
			})
			ctx := t.Context()
			if scenario == "hold" {
				ctx = netproxy.Hold(ctx, errors.New("synthetic model acquisition hold"))
			}
			if scenario == "canceled" {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			if _, _, _, check, err := PrepareExtensionAccountTask(ctx, fixture.manager, selection); err == nil || check != nil {
				t.Fatal("unavailable or superseded catalog admitted paid work", scenario, err)
			}
			wantCalls := int32(2)
			if scenario == "hold" || scenario == "canceled" {
				wantCalls = 1
			}
			if fixture.calls.Load() != wantCalls {
				t.Fatal("model acquisition retried or escaped its hold", fixture.calls.Load(), wantCalls)
			}
			if scenario == "configuration" || scenario == "account" || scenario == "package" || scenario == "disabled" {
				after, err := fixture.manager.Store().StateGet(sdk.Scope{Plugin: selection.Scope.Plugin}, pinned.modelCatalogKey())
				if err != nil {
					t.Fatal(err)
				}
				if after.Version != catalog.Version || !bytes.Equal(after.Value, catalog.Value) {
					t.Fatal("late discovery replaced the pinned catalog")
				}
			}
		})
	}
}
