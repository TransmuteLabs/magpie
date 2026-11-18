package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type toolAuthPolicyHost struct {
	*fixtureExtensionProviderHost
	revision string
}

func (host *toolAuthPolicyHost) List() ([]extensionhost.Installed, error) {
	entries, err := host.fixtureExtensionProviderHost.List()
	for index := range entries {
		entries[index].Revision = host.revision
	}
	return entries, err
}

func TestExtensionToolAuthPolicyUsesSelectedBinding(test *testing.T) {
	for _, scenario := range []string{
		"same generation", "new credentials", "stale configuration", "stale package",
		"other namespace", "sibling account", "untyped refusal", "malformed mark",
		"account off", "provider off", "unknown account", "public tool",
	} {
		test.Run(scenario, func(test *testing.T) {
			host := fixtureExtensionProvider(test, true)
			host.manifest.Contract.Version.Minor = 12
			selected := &toolAuthPolicyHost{fixtureExtensionProviderHost: host, revision: "tool-policy-1"}
			providerExtensionHost = func() extensionProviderHost { return selected }
			if quota := extensionAuthUsage(test, host, nil, &sdk.Error{Code: "unauthenticated", Message: "synthetic refusal"}); quota.Error == "" {
				test.Fatal("fixture did not publish a typed authentication refusal")
			}
			account, err := host.store.GetAccount(host.manifest.ID, "vendor", "alice")
			if err != nil {
				test.Fatal(err)
			}
			scope := sdk.Scope{Plugin: account.Plugin, Provider: account.Provider, Account: account.ID, Generation: account.Generation}
			wantError := false
			switch scenario {
			case "same generation":
				wantError = true
			case "new credentials":
				account.Auth = json.RawMessage(`{"access":"synthetic-new"}`)
				account, err = host.store.SaveAccount(account, account.Generation)
				if err != nil {
					test.Fatal(err)
				}
				scope.Generation = account.Generation
			case "stale configuration":
				host.config = "1"
			case "stale package":
				selected.revision = "tool-policy-2"
			case "other namespace":
				host.namespace = test.TempDir()
			case "sibling account":
				scope.Account = "bob"
			case "untyped refusal", "malformed mark":
				logins, err := readLoginsFile()
				if err != nil {
					test.Fatal(err)
				}
				for index := range logins {
					if logins[index].Agent == ExtensionProviderID(scope.Plugin, scope.Provider) && logins[index].User == scope.Account {
						logins[index].Refused = ""
						if scenario == "malformed mark" {
							logins[index].Refused = extensionAuthMark + "malformed"
						}
					}
				}
				if err := writeLogins(logins); err != nil {
					test.Fatal(err)
				}
				wantError = true
			case "account off":
				if err := SetLoginOn(ExtensionProviderID(scope.Plugin, scope.Provider), scope.Account, false); err != nil {
					test.Fatal(err)
				}
				wantError = true
			case "provider off":
				body, err := json.Marshal(file{Providers: []Provider{{ID: ExtensionProviderID(scope.Plugin, scope.Provider), Off: true}}})
				if err != nil {
					test.Fatal(err)
				}
				if err := os.WriteFile(Path(), body, 0600); err != nil {
					test.Fatal(err)
				}
				wantError = true
			case "unknown account":
				scope.Account = "missing"
				wantError = true
			case "public tool":
				scope.Account, scope.Generation = "", 0
			}
			calls := 0
			host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
				calls++
				return nil, errors.New("unexpected selected-tool policy dispatch")
			}
			_, prepared, err := PrepareExtensionAccountScope(test.Context(), scope)
			if (err != nil) != wantError {
				test.Errorf("selected binding admission: error=%v wantError=%v", err, wantError)
			}
			if err == nil && prepared != scope {
				test.Errorf("admission changed selected authority: got %+v want %+v", prepared, scope)
			}
			if calls != 0 {
				test.Fatalf("selected-tool admission dispatched %d module calls", calls)
			}
		})
	}
}
