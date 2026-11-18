package provider

import (
	"context"
	"encoding/json"
	"testing"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionAccountOnlyCatalogNeverInvokesSiblingDiscovery(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := "service"
		if mixed {
			name = "mixed"
		}
		t.Run(name, func(t *testing.T) {
			host := fixtureExtensionProvider(t, true)
			host.manifest.Contract.Version.Minor = 11
			if !mixed {
				host.manifest.Providers = nil
				host.manifest.Contract.Capabilities = []sdk.Capability{sdk.AccountAuth, sdk.UsageReporter}
			}
			host.manifest.Providers = append(host.manifest.Providers, sdk.ProviderDescriptor{ID: "service", Name: "Service"})
			if err := host.manifest.Validate(); err != nil {
				t.Fatal(err)
			}
			account, err := host.Store().SaveAccount(extensionhost.Account{Plugin: host.manifest.ID, Provider: "service", ID: "alice",
				Auth: json.RawMessage(`{"token":"private"}`)}, 0)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			host.call = func(_ context.Context, method string, scope sdk.Scope, _ any, _ func(extensionhost.Event) error) (json.RawMessage, error) {
				calls++
				if scope.Provider != "vendor" || method != sdk.MethodProviderModels {
					t.Fatal("service account invoked model discovery")
				}
				return json.RawMessage(`{"models":[{"id":"sibling"}]}`), nil
			}
			all, err := ExtensionProviders()
			if err != nil {
				t.Fatal(err)
			}
			var service *Provider
			for _, current := range all {
				scope, _ := current.ExtensionScope()
				if scope.Provider == "service" {
					service = &current
				} else if mixed {
					if _, err := current.Account.fetch(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
			}
			if service == nil || mixed && calls == 0 {
				t.Fatal("provider projection lost service or inference sibling")
			}
			before := calls
			if models, err := service.Account.fetch(t.Context()); err != nil || len(models) != 0 || len(service.Account.models()) != 0 {
				t.Fatal("service catalog refresh must produce no models:", models, err)
			}
			host.config = "1"
			if _, err := service.Account.fetch(t.Context()); err == nil {
				t.Fatal("old service binding ignored replacement configuration")
			}
			host.config = "0"
			if _, err := host.Store().SaveAccount(account, account.Generation); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Account.fetch(t.Context()); err == nil {
				t.Fatal("old service binding ignored replaced account")
			}
			host.disabled = true
			if _, err := service.Account.fetch(t.Context()); err == nil || calls != before {
				t.Fatal("disabled or stale service reached inference handler:", err)
			}
		})
	}
}
