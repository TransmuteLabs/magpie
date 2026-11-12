package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func extensionAuxiliaryCall(ctx context.Context, binding *extensionAccount, method string) error {
	switch method {
	case sdk.MethodProviderModels:
		_, err := binding.fetchModels(ctx)
		return err
	case sdk.MethodProviderUsage:
		quota := extensionQuota(ctx, Login{Agent: ExtensionProviderID(binding.scope.Plugin, binding.scope.Provider),
			User: binding.scope.Account, extension: binding})
		if quota.Error != "" {
			return errors.New(quota.Error)
		}
		return nil
	default:
		return extensionAuthCall(ctx, binding.scope, method, struct{}{}, nil)
	}
}

func TestExtensionProviderAuxiliaryCallsUseAccountNetworkPolicy(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	providers, err := ExtensionProviders()
	if err != nil {
		t.Fatal(err)
	}
	bindings := map[string]*extensionAccount{}
	for _, provider := range providers {
		bindings[provider.Account.User] = provider.Account.extension
	}
	pending := *bindings["alice"]
	pending.scope.Account, pending.scope.Generation = "", 0
	bindings[""] = &pending
	const providerProxy = "http://127.0.0.1:18080"
	const globalProxy = "http://127.0.0.1:28080"
	setProxies(t, providers[0].ID, providerProxy, map[string]string{"alice": "@extension-fixture", "bob": "direct"})
	s := settings.Load()
	s.Proxy = globalProxy
	s.Proxies = []settings.NamedProxy{{ID: "extension-fixture", Name: "Fixture", URL: "socks5://127.0.0.1:1080",
		User: "proxy-user", Pass: "proxy-pass"}}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	type markerKey struct{}
	parent := context.WithValue(context.Background(), markerKey{}, "parent request")
	parent = netproxy.With(parent, "http://127.0.0.1:38080")
	parent, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	deadline, _ := parent.Deadline()
	for _, policy := range []struct {
		account, choice, proxy string
	}{
		{"alice", "@extension-fixture", "socks5://proxy-user:proxy-pass@127.0.0.1:1080"},
		{"bob", "direct", ""},
		{"", providerProxy, providerProxy},
		{"alice", "", globalProxy},
	} {
		if policy.choice == "" {
			setProxies(t, providers[0].ID, "", nil)
		}
		for _, method := range []string{sdk.MethodAuthStart, sdk.MethodAuthFinish, sdk.MethodAuthRefresh,
			sdk.MethodAuthLogout, sdk.MethodProviderModels, sdk.MethodProviderUsage} {
			if policy.account == "" && method != sdk.MethodAuthStart {
				continue
			}
			calls := 0
			host.call = func(ctx context.Context, got string, scope sdk.Scope, params any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
				calls++
				if got != method || scope.Account != policy.account || netproxy.Choice(ctx) != policy.choice {
					t.Fatalf("%s account %q: scope=%+v proxy=%q", got, policy.account, scope, netproxy.Choice(ctx))
				}
				if actual, ok := ctx.Deadline(); !ok || actual != deadline || ctx.Value(markerKey{}) != "parent request" {
					t.Fatal("extension call lost its parent request context")
				}
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://203.0.113.10/resource", nil)
				if err != nil {
					t.Fatal(err)
				}
				proxy, err := netproxy.Func(request)
				effective := ""
				if proxy != nil {
					effective = proxy.String()
				}
				if err != nil || effective != policy.proxy {
					t.Fatalf("effective proxy=%q error=%v want=%q", effective, err, policy.proxy)
				}
				raw, err := json.Marshal(params)
				if err != nil {
					t.Fatal(err)
				}
				var requestParams map[string]json.RawMessage
				if json.Unmarshal(raw, &requestParams) != nil || len(requestParams["credentials"]) > 0 &&
					string(requestParams["credentials"]) != "null" {
					t.Fatalf("credentials escaped Frame.Auth: %s", raw)
				}
				switch got {
				case sdk.MethodProviderModels:
					return json.RawMessage(`{"models":[{"id":"m1","protocols":["chat"]}]}`), nil
				case sdk.MethodProviderUsage:
					return json.RawMessage(`{"usage":{},"windows":[{"name":"Weekly","used":25}]}`), nil
				default:
					return json.RawMessage(`{}`), nil
				}
			}
			if err := extensionAuxiliaryCall(parent, bindings[policy.account], method); err != nil {
				t.Fatalf("%s account %q: %v", method, policy.account, err)
			}
			if calls != 1 {
				t.Fatalf("%s invoked the module %d times", method, calls)
			}
		}
	}
}

func TestExtensionProviderHeldAuxiliaryCallsDoNotInvokeModule(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	binding := extensionAccounts()[0].Account.extension
	held := errors.New("test: allowance reads require an explicit request")
	ctx := netproxy.Hold(context.Background(), held)
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		t.Fatal("held work invoked the extension module")
		return nil, nil
	}
	for _, method := range []string{sdk.MethodAuthStart, sdk.MethodAuthFinish, sdk.MethodAuthRefresh,
		sdk.MethodAuthLogout, sdk.MethodProviderModels, sdk.MethodProviderUsage} {
		if err := extensionAuxiliaryCall(ctx, binding, method); err == nil || err.Error() != held.Error() {
			t.Fatalf("%s did not preserve the network hold: %v", method, err)
		}
	}
	quota := extensionQuota(ctx, Login{Agent: ExtensionProviderID(binding.scope.Plugin, binding.scope.Provider),
		User: binding.scope.Account, extension: binding})
	if quota.Error != held.Error() || !quota.ReadDeferred {
		t.Fatalf("held usage was not marked deferred: %+v", quota)
	}
}
