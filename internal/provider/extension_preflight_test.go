package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func expiringExtensionFixture(t *testing.T) (*fixtureExtensionProviderHost, Provider) {
	t.Helper()
	host := fixtureExtensionProvider(t, true)
	account, err := host.store.GetAccount(host.manifest.ID, "vendor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	account.ExpiresAt = time.Now().Add(5 * time.Second).UnixMilli()
	if _, err := host.store.SaveAccount(account, account.Generation); err != nil {
		t.Fatal(err)
	}
	return host, extensionAccounts()[0]
}

func TestExtensionProviderRefreshesBeforeExpiredAccountWork(t *testing.T) {
	for _, method := range []string{sdk.MethodProviderExecute, sdk.MethodProviderModels, sdk.MethodProviderUsage} {
		t.Run(method, func(t *testing.T) {
			host, provider := expiringExtensionFixture(t)
			var refreshes atomic.Int32
			host.refresh = func(_ context.Context, id string, scope sdk.Scope) (sdk.AuthResult, error) {
				refreshes.Add(1)
				if id != host.manifest.ID || scope.Account != "alice" || scope.Generation != 2 {
					t.Errorf("refresh selected the wrong credential generation: %+v", scope)
				}
				_, err := host.store.SaveAccount(extensionhost.Account{Plugin: id, Provider: "vendor", ID: "alice",
					Auth: json.RawMessage(`{"access":"rotated"}`), ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, scope.Generation)
				return sdk.AuthResult{ID: "alice"}, err
			}
			var calls atomic.Int32
			host.call = func(_ context.Context, got string, scope sdk.Scope, params any, consume func(extensionhost.Event) error) (json.RawMessage, error) {
				calls.Add(1)
				account, err := host.store.GetAccount(scope.Plugin, scope.Provider, scope.Account)
				if err != nil || got != method || scope.Generation != 3 || account.Generation != 3 ||
					string(account.Auth) != `{"access":"rotated"}` {
					t.Errorf("work dispatched before durable refreshed credentials: %s %+v %v", got, scope, err)
				}
				switch got {
				case sdk.MethodProviderExecute:
					if err := consume(extensionhost.Event{Name: "headers", Data: json.RawMessage(`{"status":200}`)}); err != nil {
						return nil, err
					}
					if err := consume(extensionhost.Event{Name: "data", Data: json.RawMessage(`"e30="`)}); err != nil {
						return nil, err
					}
					return json.RawMessage(`{"status":200}`), nil
				case sdk.MethodProviderModels:
					return json.RawMessage(`{"models":[{"id":"m1"}]}`), nil
				default:
					return json.RawMessage(`{"usage":{},"windows":[{"name":"Plan","used":0}]}`), nil
				}
			}
			invoke := func(selected Provider) {
				t.Helper()
				switch method {
				case sdk.MethodProviderExecute:
					request, _ := http.NewRequest(http.MethodPost, selected.Chat+"/chat/completions", strings.NewReader(`{"model":"m1"}`))
					response, err := selected.Do(http.DefaultClient, request)
					if err != nil {
						t.Fatal(err)
					}
					_, err = io.ReadAll(response.Body)
					response.Body.Close()
					if err != nil {
						t.Fatal(err)
					}
				case sdk.MethodProviderModels:
					if _, err := selected.Account.fetch(context.Background()); err != nil {
						t.Fatal(err)
					}
				default:
					quota := extensionQuota(context.Background(), Login{Agent: selected.ID, User: selected.Account.User,
						extension: selected.Account.extension})
					if quota.Error != "" {
						t.Fatal(quota.Error)
					}
				}
			}
			invoke(provider)
			invoke(provider)
			invoke(extensionAccounts()[0])
			if calls.Load() != 3 || refreshes.Load() != 1 {
				t.Fatalf("credential preflight repeated or skipped work: calls=%d refreshes=%d", calls.Load(), refreshes.Load())
			}
		})
	}
}

func TestExtensionProviderRefreshPreflightFailsClosed(t *testing.T) {
	for _, scenario := range []string{"held", "uncertain", "short expiry", "manual replacement"} {
		t.Run(scenario, func(t *testing.T) {
			host, provider := expiringExtensionFixture(t)
			var refreshes atomic.Int32
			host.refresh = func(_ context.Context, id string, scope sdk.Scope) (sdk.AuthResult, error) {
				refreshes.Add(1)
				if scenario == "uncertain" {
					return sdk.AuthResult{}, &extensionhost.OutcomeError{Outcome: "uncertain"}
				}
				_, err := host.store.SaveAccount(extensionhost.Account{Plugin: id, Provider: "vendor", ID: "alice",
					Auth: json.RawMessage(`{"access":"too-short"}`), ExpiresAt: time.Now().Add(time.Second).UnixMilli()}, scope.Generation)
				return sdk.AuthResult{ID: "alice"}, err
			}
			host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
				t.Error("unusable credential generation reached ordinary work")
				return nil, nil
			}
			ctx := context.Background()
			if scenario == "held" {
				ctx = netproxy.Hold(ctx, errors.New("fixture network hold"))
			}
			if scenario == "manual replacement" {
				_, err := host.store.SaveAccount(extensionhost.Account{Plugin: host.manifest.ID, Provider: "vendor", ID: "alice",
					Auth: json.RawMessage(`{"access":"manual"}`)}, 2)
				if err != nil {
					t.Fatal(err)
				}
			}
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, provider.Chat+"/chat/completions", strings.NewReader(`{"model":"m1"}`))
			if _, err := provider.Do(http.DefaultClient, request); err == nil {
				t.Fatal("unusable credentials were admitted")
			}
			if (scenario == "held" || scenario == "manual replacement") && refreshes.Load() != 0 {
				t.Fatal("network hold or superseded credentials triggered a refresh")
			}
		})
	}
}
