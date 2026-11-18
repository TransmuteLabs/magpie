package provider

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type oldRefreshFixture struct{ extensionProviderHost }

func TestExtensionAuthRefreshRequiresTransactionOwner(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	providerExtensionHost = func() extensionProviderHost { return oldRefreshFixture{host} }
	called := false
	host.call = func(context.Context, string, sdk.Scope, any, func(extensionhost.Event) error) (json.RawMessage, error) {
		called = true
		return nil, errors.New("unsafe fallback")
	}
	_, err := RefreshExtensionAuth(context.Background(), sdk.Scope{
		Plugin: host.manifest.ID, Provider: "vendor", Account: "alice", Generation: 1,
	})
	if err == nil || called {
		t.Fatalf("host without transaction used unsafe fallback: %v", err)
	}
}

func TestExtensionAuthRefreshKeepsCapturedStore(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	other := &fixtureExtensionProviderHost{manifest: host.manifest, store: extensionhost.NewStore(t.TempDir())}
	scope := sdk.Scope{Plugin: host.manifest.ID, Provider: "vendor", Account: "alice", Generation: 1}
	account := extensionhost.Account{
		Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account,
		Auth: json.RawMessage(`{"token":"other-store"}`),
	}
	if _, err := other.store.SaveAccount(account, 0); err != nil {
		t.Fatal(err)
	}
	host.refresh = func(ctx context.Context, id string, got sdk.Scope) (sdk.AuthResult, error) {
		providerExtensionHost = func() extensionProviderHost { return other }
		account.Auth = json.RawMessage(`{"token":"rotated-original"}`)
		_, err := host.store.SaveAccount(account, 1)
		return sdk.AuthResult{ID: scope.Account}, err
	}
	result, err := RefreshExtensionAuth(context.Background(), scope)
	if err != nil || result.ID != scope.Account {
		t.Fatalf("refresh = %+v %v", result, err)
	}
	current, err := other.store.GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil || current.Generation != 1 || string(current.Auth) != `{"token":"other-store"}` {
		t.Fatalf("refresh wrote into replacement default host: generation %d %v", current.Generation, err)
	}
}

func TestExtensionAuthRefreshNeverReturnsCredentialsWithError(t *testing.T) {
	host := fixtureExtensionProvider(t, true)
	failure := errors.New("synthetic failure")
	host.refresh = func(context.Context, string, sdk.Scope) (sdk.AuthResult, error) {
		return sdk.AuthResult{Credentials: json.RawMessage(`{"token":"private"}`)}, failure
	}
	result, err := RefreshExtensionAuth(context.Background(), sdk.Scope{
		Plugin: host.manifest.ID, Provider: "vendor", Account: "alice", Generation: 1,
	})
	if !errors.Is(err, failure) || len(result.Credentials) != 0 {
		t.Fatalf("refresh error exposed credentials: %v", err)
	}
}
