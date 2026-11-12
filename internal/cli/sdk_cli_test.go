package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/steady"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

type sdkCLITestHost struct {
	entries  []extensionhost.Installed
	store    *extensionhost.Store
	calls    []string
	scope    sdk.Scope
	params   any
	closed   int
	closeErr error
	call     func(context.Context, string, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error)
	invoke   func(string, sdk.Scope, any) (any, error)
	listErr  error
}

func sdkCLIHostForTest(t *testing.T) *sdkCLITestHost {
	t.Helper()
	helpHome(t)
	manager := extensionhost.Default()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	host := &sdkCLITestHost{
		store: manager.Store(),
		entries: []extensionhost.Installed{{
			Manifest: sdk.Manifest{Schema: 1, ID: "sample", Version: "1.0.0", Runtime: "exec", Entry: "worker",
				Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1},
					Capabilities: []sdk.Capability{sdk.AgentWiring, sdk.SessionMigration, sdk.AccountAuth, sdk.OutboundExecutor}},
				Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Vendor", Protocols: []protocolext.Protocol{"chat"}}},
				Agents:    []sdk.AgentDescriptor{{ID: "editor", Name: "Editor"}},
				Sessions:  []sdk.SessionDescriptor{{ID: "conversation", Name: "Conversation"}},
			},
			Directory: "packages/sample/1.0.0-fixture", Revision: "fixture",
		}},
	}
	source := t.TempDir()
	manifest, err := json.Marshal(host.entries[0].Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "worker"), []byte("fixture, never executed"), 0o700); err != nil {
		t.Fatal(err)
	}
	entry, err := extensionhost.NewCatalog(manager.Namespace()).Install(t.Context(), source, func(context.Context, extensionhost.Installed) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	host.entries[0] = entry
	return host
}

func (host *sdkCLITestHost) List() ([]extensionhost.Installed, error) {
	return host.entries, host.listErr
}

func (host *sdkCLITestHost) Manifest(id string) (sdk.Manifest, error) {
	for _, entry := range host.entries {
		if entry.Manifest.ID == id && !entry.Disabled {
			return entry.Manifest.Public(), nil
		}
	}
	return sdk.Manifest{}, errors.New("module unavailable")
}

func (host *sdkCLITestHost) Store() *extensionhost.Store { return host.store }

func (host *sdkCLITestHost) Install(ctx context.Context, source string) (extensionhost.Installed, error) {
	host.calls = append(host.calls, "install:"+source)
	return host.entries[0], ctx.Err()
}

func (host *sdkCLITestHost) SetEnabled(ctx context.Context, id string, on bool) error {
	host.calls = append(host.calls, fmt.Sprintf("enabled:%s:%t", id, on))
	return ctx.Err()
}

func (host *sdkCLITestHost) Remove(ctx context.Context, id string) error {
	host.calls = append(host.calls, "remove:"+id)
	return ctx.Err()
}

func (host *sdkCLITestHost) Restart(ctx context.Context, id string) error {
	host.calls = append(host.calls, "restart:"+id)
	return ctx.Err()
}

func (host *sdkCLITestHost) Call(ctx context.Context, id, method string, scope sdk.Scope, params any, consume func(extensionhost.Event) error) (json.RawMessage, error) {
	host.calls = append(host.calls, method)
	host.scope, host.params = scope, params
	if host.call != nil {
		return host.call(ctx, id, method, scope, params, consume)
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (host *sdkCLITestHost) Invoke(ctx context.Context, id, method string, scope sdk.Scope, params, out any, consume func(extensionhost.Event) error) error {
	host.calls = append(host.calls, method)
	host.scope, host.params = scope, params
	if host.invoke == nil {
		return errors.New("unexpected action invocation")
	}
	value, err := host.invoke(method, scope, params)
	if err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (host *sdkCLITestHost) Close(ctx context.Context) error {
	host.closed++
	deadline, present := ctx.Deadline()
	if !present || time.Until(deadline) > 5*time.Second || ctx.Err() != nil {
		return errors.New("CLI close must use a fresh bounded context")
	}
	return host.closeErr
}

func sdkCLIRunForTest(t *testing.T, host *sdkCLITestHost, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := sdkPluginRun(t.Context(), args, host, &out, sdkCLIAuth{})
	return out.String(), err
}

func TestSDKCLIManagementAndHelp(t *testing.T) {
	for _, test := range []struct {
		args []string
		call string
		text string
	}{
		{nil, "", "usage: magpie plugin sdk"},
		{[]string{"help"}, "", "usage: magpie plugin sdk"},
		{[]string{"install", "--help"}, "", "usage: magpie plugin sdk"},
		{[]string{"list"}, "", "sample\t1.0.0\texec\ton"},
		{[]string{"add", "./dist"}, "install:./dist", "installed sample"},
		{[]string{"install", "./dist"}, "install:./dist", "installed sample"},
		{[]string{"on", "sample"}, "enabled:sample:true", "sample on"},
		{[]string{"off", "sample"}, "enabled:sample:false", "sample off"},
		{[]string{"rm", "sample"}, "remove:sample", "sample rm"},
		{[]string{"reload", "sample"}, "restart:sample", "sample reload"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			host := sdkCLIHostForTest(t)
			out, err := sdkCLIRunForTest(t, host, test.args...)
			if err != nil || !strings.Contains(out, test.text) {
				t.Fatalf("output = %q, error = %v", out, err)
			}
			var want []string
			if test.call != "" {
				want = []string{test.call}
			}
			if !reflect.DeepEqual(host.calls, want) || host.closed != 1 {
				t.Fatalf("calls=%v, closes=%d", host.calls, host.closed)
			}
		})
	}
	host := sdkCLIHostForTest(t)
	host.entries[0].Disabled = true
	out, err := sdkCLIRunForTest(t, host, "inspect", "sample")
	var inspected extensionhost.Installed
	if err != nil || json.Unmarshal([]byte(out), &inspected) != nil || !inspected.Disabled {
		t.Fatalf("inspect hid a disabled module: %q, %v", out, err)
	}
	host = sdkCLIHostForTest(t)
	host.entries = nil
	out, err = sdkCLIRunForTest(t, host, "list", "--json")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty JSON list = %q, %v", out, err)
	}
	host = sdkCLIHostForTest(t)
	host.listErr = errors.New("catalog is unreadable")
	out, err = sdkCLIRunForTest(t, host, "list", "--json")
	if !errors.Is(err, host.listErr) || out != "" {
		t.Fatalf("bad catalog read became output: %q, %v", out, err)
	}
}

func TestSDKCLIRejectsBadArgumentsBeforeDispatch(t *testing.T) {
	for _, args := range [][]string{
		{"unknown"}, {"list", "sample"}, {"list", "--bogus"}, {"add"}, {"off"}, {"rm", "first", "second"},
		{"reload"}, {"inspect", "missing"}, {"call", "sample", "tool.call"},
		{"call", "sample", "tool.call", "{"}, {"call", "sample", "tool.call", "{}", "--apply"},
		{"call", "sample", "tool.call", "{}", "--scope", "null"},
		{"call", "sample", "tool.call", "{}", "--scope", `{"unknown":1}`},
		{"call", "sample", "tool.call", "{}", "--scope", `{"plugin":"other"}`},
		{"call", "sample", "tool.call", "{}", "--scope", `{"generation":1}`},
		{"call", "sample", "tool.call", "{}", "--scope", `{"account":"someone"}`},
		{"list", "--json", "--json"}, {"list", "--json=true"}, {"call", "sample", "tool.call", "{}", "--scope"},
		{"accounts", "sample"}, {"accounts", "sample", "other"}, {"auth"}, {"auth", "unknown"},
		{"auth", "start", "sample", "vendor", "null"}, {"auth", "finish", "sample", "vendor", `{"unknown":true}`},
		{"agent", "sample", "{}"}, {"session", "sample", "{}", "--root", "relative", "--path", "target"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			host := sdkCLIHostForTest(t)
			out, err := sdkCLIRunForTest(t, host, args...)
			if err == nil || out != "" || len(host.calls) != 0 || host.closed != 1 {
				t.Fatalf("invalid invocation dispatched: output=%q, err=%v, calls=%v, closes=%d", out, err, host.calls, host.closed)
			}
		})
	}
}

func TestSDKCLICallStreamsAndResolvesAccountGeneration(t *testing.T) {
	for _, command := range []string{"call", "invoke"} {
		t.Run(command, func(t *testing.T) {
			host := sdkCLIHostForTest(t)
			account, err := host.store.SaveAccount(extensionhost.Account{
				Plugin: "sample", Provider: "vendor", ID: "account", Auth: json.RawMessage(`{"token":"private"}`),
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			host.call = func(ctx context.Context, id, method string, scope sdk.Scope, params any, consume func(extensionhost.Event) error) (json.RawMessage, error) {
				if scope.Generation != account.Generation || scope.Plugin != "sample" || scope.Account != "account" {
					return nil, errors.New("unresolved account scope")
				}
				if err := consume(extensionhost.Event{Name: "data", Data: json.RawMessage(`{"text":"hello"}`)}); err != nil {
					return nil, err
				}
				return json.RawMessage(`{"finished":true}`), nil
			}
			out, err := sdkCLIRunForTest(t, host, command, "sample", "provider.execute", `{"body":"AA=="}`,
				"--scope", `{"provider":"vendor","account":"account","operation":"op"}`)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if len(lines) != 2 || !strings.Contains(lines[0], `"kind":"event"`) || !strings.Contains(lines[1], `"kind":"result"`) {
				t.Fatalf("stream was not event/result JSON: %q", out)
			}
			for _, line := range lines {
				if !json.Valid([]byte(line)) {
					t.Fatalf("invalid streamed JSON: %q", line)
				}
			}
			if string(host.params.(json.RawMessage)) != `{"body":"AA=="}` || host.scope.Operation != "op" {
				t.Fatalf("call changed its input or scope: %v, %+v", host.params, host.scope)
			}
		})
	}
}

func sdkCLITestNetworkPolicy(t *testing.T, proxy string, accounts map[string]string) {
	t.Helper()
	data, err := json.Marshal(struct {
		Providers []provider.Provider `json:"providers"`
	}{[]provider.Provider{{ID: provider.ExtensionProviderID("sample", "vendor"), Proxy: proxy, AccountProxies: accounts}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(provider.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := steady.WriteFile(provider.Path(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	options := settings.Load()
	options.Proxy = "http://127.0.0.1:28080"
	options.Proxies = []settings.NamedProxy{{ID: "sdk-fixture", Name: "SDK fixture", URL: "socks5://127.0.0.1:1080",
		User: "proxy-user", Pass: "proxy-pass"}}
	if err := settings.Save(options); err != nil {
		t.Fatal(err)
	}
}

func TestSDKCLICallAppliesSelectedNetworkPolicy(t *testing.T) {
	for _, policy := range []struct {
		name, providerProxy, accountProxy, scope, choice, effective string
	}{
		{"account", "http://127.0.0.1:18080", "@sdk-fixture", `{"provider":"vendor","account":"account"}`,
			"@sdk-fixture", "socks5://proxy-user:proxy-pass@127.0.0.1:1080"},
		{"direct", "http://127.0.0.1:18080", "direct", `{"provider":"vendor","account":"account"}`, "direct", ""},
		{"provider-inherited", "http://127.0.0.1:18080", "", `{"provider":"vendor","account":"account"}`,
			"http://127.0.0.1:18080", "http://127.0.0.1:18080"},
		{"provider-without-account", "http://127.0.0.1:18080", "", `{"provider":"vendor"}`,
			"http://127.0.0.1:18080", "http://127.0.0.1:18080"},
		{"global", "", "", `{"provider":"vendor","account":"account"}`, "", "http://127.0.0.1:28080"},
		{"module-inherited", "", "", `{}`, "http://127.0.0.1:38080", "http://127.0.0.1:38080"},
	} {
		t.Run(policy.name, func(t *testing.T) {
			host := sdkCLIHostForTest(t)
			if _, err := host.store.SaveAccount(extensionhost.Account{
				Plugin: "sample", Provider: "vendor", ID: "account", Auth: json.RawMessage(`{"token":"private"}`),
			}, 0); err != nil {
				t.Fatal(err)
			}
			sdkCLITestNetworkPolicy(t, policy.providerProxy, map[string]string{"account": policy.accountProxy})
			var lookups atomic.Int64
			restoreLookup := netproxy.SetLookup(func(context.Context, string) ([]net.IPAddr, error) {
				lookups.Add(1)
				return nil, errors.New("DNS is forbidden in the SDK network policy fixture")
			})
			t.Cleanup(restoreLookup)
			const inheritedEnv = "http://env-user:env-pass@127.0.0.1:48080"
			t.Setenv("HTTPS_PROXY", inheritedEnv)
			type markerKey struct{}
			parent := netproxy.With(context.WithValue(t.Context(), markerKey{}, "parent request"), "http://127.0.0.1:38080")
			parent, cancel := context.WithTimeout(parent, time.Minute)
			defer cancel()
			deadline, _ := parent.Deadline()
			var calledContext context.Context
			host.call = func(ctx context.Context, id, method string, scope sdk.Scope, params any, consume func(extensionhost.Event) error) (json.RawMessage, error) {
				calledContext = ctx
				if netproxy.Choice(ctx) != policy.choice {
					t.Fatalf("network choice=%q, want %q", netproxy.Choice(ctx), policy.choice)
				}
				if got, ok := ctx.Deadline(); !ok || got != deadline || ctx.Value(markerKey{}) != "parent request" {
					t.Fatal("network policy lost its parent context")
				}
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://203.0.113.10/resource", nil)
				if err != nil {
					t.Fatal(err)
				}
				selected, err := netproxy.Func(request)
				effective := ""
				if selected != nil {
					effective = selected.String()
				}
				if err != nil || effective != policy.effective {
					t.Fatalf("effective proxy=%q, error=%v, want %q", effective, err, policy.effective)
				}
				raw, err := json.Marshal(params)
				if err != nil || string(raw) != `{"ok":true}` || os.Getenv("HTTPS_PROXY") != inheritedEnv {
					t.Fatalf("network policy changed parameters or process environment: %s, %v", raw, err)
				}
				return json.RawMessage(`{"done":true}`), nil
			}
			var out bytes.Buffer
			err := sdkPluginRun(parent, []string{"call", "sample", "provider.models", `{"ok":true}`, "--scope", policy.scope}, host, &out, sdkCLIAuth{})
			if err != nil || calledContext == nil || host.closed != 1 {
				t.Fatalf("network-scoped call failed: %v, called=%v, closes=%d", err, calledContext != nil, host.closed)
			}
			cancel()
			if !errors.Is(calledContext.Err(), context.Canceled) {
				t.Fatal("network policy detached cancellation from its parent")
			}
			if lookups.Load() != 0 {
				t.Fatalf("proxy selection attempted %d DNS lookups", lookups.Load())
			}
			if strings.Contains(out.String(), "proxy-user") || strings.Contains(out.String(), "proxy-pass") {
				t.Fatalf("proxy credentials escaped into output: %q", out.String())
			}
		})
	}
}

func TestSDKCLIHeldNetworkPolicyDoesNotInvokeModule(t *testing.T) {
	host := sdkCLIHostForTest(t)
	if _, err := host.store.SaveAccount(extensionhost.Account{
		Plugin: "sample", Provider: "vendor", ID: "account", Auth: json.RawMessage(`{}`),
	}, 0); err != nil {
		t.Fatal(err)
	}
	held := errors.New("SDK network policy fixture hold")
	ctx := netproxy.Hold(netproxy.With(t.Context(), "http://127.0.0.1:38080"), held)
	var out bytes.Buffer
	err := sdkPluginRun(ctx, []string{"call", "sample", "provider.execute", `{}`,
		"--scope", `{"provider":"vendor","account":"account"}`}, host, &out, sdkCLIAuth{})
	if !errors.Is(err, held) || len(host.calls) != 0 || out.Len() != 0 || host.closed != 1 {
		t.Fatalf("held call invoked the module: err=%v, calls=%v, output=%q, closes=%d", err, host.calls, out.String(), host.closed)
	}
}

func TestSDKCLIExtensionNetworkContextRejectsInvalidScopes(t *testing.T) {
	for _, name := range []string{"module", "provider", "missing-account", "missing-generation", "stale-generation", "orphan-generation", "missing-provider", "disabled"} {
		t.Run(name, func(t *testing.T) {
			host := sdkCLIHostForTest(t)
			account, err := host.store.SaveAccount(extensionhost.Account{
				Plugin: "sample", Provider: "vendor", ID: "account", Auth: json.RawMessage(`{}`),
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			scope := sdk.Scope{Plugin: "sample", Provider: "vendor", Account: "account", Generation: account.Generation}
			switch name {
			case "module":
				scope.Plugin = "other"
			case "provider":
				scope.Provider = "other"
			case "missing-account":
				scope.Account = "other"
			case "missing-generation":
				scope.Generation = 0
			case "stale-generation":
				if _, err := host.store.SaveAccount(account, account.Generation); err != nil {
					t.Fatal(err)
				}
			case "orphan-generation":
				scope.Account = ""
			case "missing-provider":
				scope.Provider = ""
			case "disabled":
				if err := extensionhost.Default().SetEnabled(t.Context(), "sample", false); err != nil {
					t.Fatal(err)
				}
			}
			ctx := netproxy.With(t.Context(), "http://127.0.0.1:38080")
			result, err := provider.ExtensionNetworkContext(ctx, scope)
			if err == nil || result != ctx {
				t.Fatalf("invalid network scope changed its context: %+v, %v", scope, err)
			}
		})
	}
}

func TestSDKCLIAccountsNeverPrintCredentials(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		host := sdkCLIHostForTest(t)
		if _, err := host.store.SaveAccount(extensionhost.Account{
			Plugin: "sample", Provider: "vendor", ID: "account", Auth: json.RawMessage(`{"access_token":"sdk-secret-value"}`),
		}, 0); err != nil {
			t.Fatal(err)
		}
		args := []string{"accounts", "sample", "vendor"}
		if asJSON {
			args = append(args, "--json")
		}
		out, err := sdkCLIRunForTest(t, host, args...)
		if err != nil || !strings.Contains(out, "account") || strings.Contains(out, "auth") ||
			strings.Contains(out, "access_token") || strings.Contains(out, "sdk-secret-value") {
			t.Fatalf("account view leaked credentials: %q, %v", out, err)
		}
	}
}

func TestSDKCLIAuthUsesSavedGenerationAndHidesCredentials(t *testing.T) {
	for _, operation := range []string{"start", "finish", "refresh", "logout"} {
		t.Run(operation, func(t *testing.T) {
			host := sdkCLIHostForTest(t)
			account, err := host.store.SaveAccount(extensionhost.Account{
				Plugin: "sample", Provider: "vendor", ID: "account", Auth: json.RawMessage(`{"token":"private"}`),
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			called := ""
			check := func(scope sdk.Scope) {
				t.Helper()
				if scope.Plugin != "sample" || scope.Provider != "vendor" || scope.Account != "account" || scope.Generation != account.Generation {
					t.Fatalf("authentication used a different scope: %+v", scope)
				}
			}
			auth := sdkCLIAuth{
				start: func(ctx context.Context, scope sdk.Scope, request sdk.AuthStartRequest) (sdk.AuthStartResult, error) {
					check(scope)
					called = "start"
					if request.Method != "device" {
						t.Fatalf("start request = %+v", request)
					}
					return sdk.AuthStartResult{UserCode: "code"}, nil
				},
				finish: func(ctx context.Context, scope sdk.Scope, request sdk.AuthFinishRequest) (sdk.AuthResult, error) {
					check(scope)
					called = "finish"
					if request.Code != "code" {
						t.Fatalf("finish request = %+v", request)
					}
					return sdk.AuthResult{ID: "account", Credentials: json.RawMessage(`{"token":"must-not-print"}`)}, nil
				},
				refresh: func(ctx context.Context, scope sdk.Scope) (sdk.AuthResult, error) {
					check(scope)
					called = "refresh"
					return sdk.AuthResult{ID: "account", Credentials: json.RawMessage(`{"token":"must-not-print"}`)}, nil
				},
				logout: func(ctx context.Context, scope sdk.Scope) error {
					check(scope)
					called = "logout"
					return nil
				},
			}
			input := "account"
			if operation == "start" {
				input = `{"method":"device"}`
			}
			if operation == "finish" {
				input = `{"code":"code"}`
			}
			args := []string{"auth", operation, "sample", "vendor", input, "--scope", `{"account":"account"}`}
			var out bytes.Buffer
			err = sdkPluginRun(t.Context(), args, host, &out, auth)
			if err != nil || called != operation || strings.Contains(out.String(), "must-not-print") || strings.Contains(out.String(), "credentials") {
				t.Fatalf("auth command = %q, output=%q, err=%v", called, out.String(), err)
			}
		})
	}
}

func TestSDKCLIAuthRejectsStaleAndMismatchedScopes(t *testing.T) {
	for _, scope := range []string{
		`{"generation":1}`, `{"provider":"other"}`, `{"plugin":"other"}`, `{"account":"other"}`,
	} {
		host := sdkCLIHostForTest(t)
		account, err := host.store.SaveAccount(extensionhost.Account{
			Plugin: "sample", Provider: "vendor", ID: "account", Auth: json.RawMessage(`{}`),
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := host.store.SaveAccount(account, account.Generation); err != nil {
			t.Fatal(err)
		}
		called := false
		var out bytes.Buffer
		err = sdkPluginRun(t.Context(), []string{"auth", "refresh", "sample", "vendor", "account", "--scope", scope}, host, &out, sdkCLIAuth{
			refresh: func(context.Context, sdk.Scope) (sdk.AuthResult, error) {
				called = true
				return sdk.AuthResult{}, nil
			},
		})
		if err == nil || called || out.Len() != 0 {
			t.Fatalf("stale or mismatched authentication scope dispatched: scope=%s, called=%v, err=%v", scope, called, err)
		}
	}
}

func TestSDKCLIActionPreviewAndExplicitApply(t *testing.T) {
	for _, command := range []string{"agent", "session"} {
		for _, apply := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/apply=%t", command, apply), func(t *testing.T) {
				host := sdkCLIHostForTest(t)
				root := t.TempDir()
				target := filepath.Join(root, "target.json")
				if err := os.WriteFile(target, []byte("before"), 0o600); err != nil {
					t.Fatal(err)
				}
				host.invoke = func(method string, scope sdk.Scope, params any) (any, error) {
					if command == "agent" {
						if method != sdk.MethodAgentPlan {
							t.Fatalf("wrong action method: %s", method)
						}
						return sdk.AgentPlan{Files: []sdk.FileChange{{Path: "target.json", Body: []byte("after")}}}, nil
					}
					if method != sdk.MethodSessionConvert {
						t.Fatalf("wrong conversion method: %s", method)
					}
					return sdk.SessionResult{Files: []sdk.SessionDocument{{Path: "target.json", Format: "target", Body: []byte("after")}}}, nil
				}
				input := `{"id":"editor","action":"connect"}`
				if command == "session" {
					input = `{"id":"conversation","from":"source","to":"target","files":[{"path":"input.json","format":"source","body":"e30="}]}`
				}
				backup := filepath.Join(t.TempDir(), "backups")
				args := []string{command, "sample", input, "--root", root, "--path", "target.json"}
				if apply {
					args = append(args, "--apply", "--backup-dir", backup)
				}
				out, err := sdkCLIRunForTest(t, host, args...)
				if err != nil || !json.Valid([]byte(out)) {
					t.Fatalf("action output=%q, err=%v", out, err)
				}
				data, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				want := "before"
				if apply {
					want = "after"
				}
				if string(data) != want {
					t.Fatalf("apply=%t left target %q, want %q", apply, data, want)
				}
				if apply {
					var result extensionhost.ActionResult
					if err := json.Unmarshal([]byte(out), &result); err != nil || !result.Applied || result.Backup == "" {
						t.Fatalf("explicit apply did not retain a backup: %+v, %v", result, err)
					}
				} else {
					entries, err := os.ReadDir(root)
					if err != nil || len(entries) != 1 {
						t.Fatalf("preview wrote into its target root: %v, %v", entries, err)
					}
					if _, err := os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("preview created a backup: %v", err)
					}
				}
			})
		}
	}
}

func TestSDKCLIActionRequiresBackupAndAuthorizedPaths(t *testing.T) {
	for _, options := range [][]string{
		{"--apply"}, {"--path", "../outside"}, {"--root", "relative"}, {"--backup-dir", "relative", "--apply"},
	} {
		host := sdkCLIHostForTest(t)
		args := []string{"agent", "sample", `{"id":"editor","action":"connect"}`, "--root", t.TempDir(), "--path", "target.json"}
		args = append(args, options...)
		out, err := sdkCLIRunForTest(t, host, args...)
		if err == nil || out != "" || len(host.calls) != 0 {
			t.Fatalf("invalid action policy invoked the module: %v, output=%q, err=%v", options, out, err)
		}
	}
}

type sdkCLIFailedWriter struct{ err error }

func (writer sdkCLIFailedWriter) Write([]byte) (int, error) { return 0, writer.err }

func TestSDKCLICloseAfterCancellationAndOutputErrors(t *testing.T) {
	host := sdkCLIHostForTest(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := sdkPluginRun(ctx, []string{"list"}, host, io.Discard, sdkCLIAuth{})
	if !errors.Is(err, context.Canceled) || host.closed != 1 {
		t.Fatalf("cancellation skipped cleanup: %v, closes=%d", err, host.closed)
	}
	host = sdkCLIHostForTest(t)
	writeErr, closeErr := errors.New("output failed"), errors.New("close failed")
	host.closeErr = closeErr
	host.call = func(ctx context.Context, id, method string, scope sdk.Scope, params any, consume func(extensionhost.Event) error) (json.RawMessage, error) {
		return nil, consume(extensionhost.Event{Name: "data", Data: json.RawMessage(`{}`)})
	}
	err = sdkPluginRun(t.Context(), []string{"call", "sample", "tool.call", "{}"}, host, sdkCLIFailedWriter{writeErr}, sdkCLIAuth{})
	if !errors.Is(err, writeErr) || !errors.Is(err, closeErr) || host.closed != 1 {
		t.Fatalf("output failure did not retain errors and cleanup: %v, closes=%d", err, host.closed)
	}
}
