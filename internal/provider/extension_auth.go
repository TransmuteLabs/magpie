package provider

import (
	"context"
	"encoding/json"
	"errors"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func extensionAuthCall(ctx context.Context, scope sdk.Scope, method string, params, out any) error {
	if settings.AccountDiscoveryOff() {
		return errAccountDiscoveryOff
	}
	host := providerExtensionHost()
	manifest, err := host.Manifest(scope.Plugin)
	if err != nil {
		return err
	}
	if !manifest.Contract.Has(sdk.AccountAuth) {
		return errors.New("extension provider does not declare account authentication")
	}
	found := false
	for _, descriptor := range manifest.Providers {
		found = found || descriptor.ID == scope.Provider
	}
	if !found {
		return errors.New("extension provider is unavailable")
	}
	if scope.Account != "" {
		account, err := host.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
		if err != nil {
			return err
		}
		if account.Generation != scope.Generation {
			return errors.New("extension account generation changed")
		}
	}
	ctx, err = extensionNetworkContext(ctx, scope)
	if err != nil {
		return err
	}
	raw, err := host.Call(ctx, scope.Plugin, method, scope, params, nil)
	if err != nil {
		return err
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return errors.New("extension provider returned an invalid authentication result")
	}
	return nil
}

func StartExtensionAuth(ctx context.Context, scope sdk.Scope, request sdk.AuthStartRequest) (sdk.AuthStartResult, error) {
	var result sdk.AuthStartResult
	err := extensionAuthCall(ctx, scope, sdk.MethodAuthStart, request, &result)
	return result, err
}

func FinishExtensionAuth(ctx context.Context, scope sdk.Scope, request sdk.AuthFinishRequest) (sdk.AuthResult, error) {
	var result sdk.AuthResult
	if err := extensionAuthCall(ctx, scope, sdk.MethodAuthFinish, request, &result); err != nil {
		return sdk.AuthResult{}, err
	}
	return saveExtensionAuth(scope, result)
}

func RefreshExtensionAuth(ctx context.Context, scope sdk.Scope) (sdk.AuthResult, error) {
	if scope.Account == "" || scope.Generation == 0 {
		return sdk.AuthResult{}, errors.New("an explicit extension account generation is required")
	}
	var result sdk.AuthResult
	if err := extensionAuthCall(ctx, scope, sdk.MethodAuthRefresh, sdk.AuthRefreshRequest{}, &result); err != nil {
		return sdk.AuthResult{}, err
	}
	return saveExtensionAuth(scope, result)
}

func saveExtensionAuth(scope sdk.Scope, result sdk.AuthResult) (sdk.AuthResult, error) {
	if result.Pending {
		result.Credentials = nil
		return result, nil
	}
	id := result.ID
	if id == "" {
		id = scope.Account
	}
	if id == "" || scope.Account != "" && scope.Account != id {
		return sdk.AuthResult{}, errors.New("extension authentication changed the selected account")
	}
	if !json.Valid(result.Credentials) || string(result.Credentials) == "null" {
		return sdk.AuthResult{}, errors.New("extension authentication returned no credentials")
	}
	_, err := providerExtensionHost().Store().SaveAccount(extensionhost.Account{
		Plugin: scope.Plugin, Provider: scope.Provider, ID: id, Auth: result.Credentials,
	}, scope.Generation)
	if err != nil {
		return sdk.AuthResult{}, err
	}
	result.ID, result.Credentials = id, nil
	Changed()
	return result, nil
}

func LogoutExtensionAuth(ctx context.Context, scope sdk.Scope) error {
	if scope.Account == "" || scope.Generation == 0 {
		return errors.New("an explicit extension account generation is required")
	}
	if err := extensionAuthCall(ctx, scope, sdk.MethodAuthLogout, sdk.AuthLogoutRequest{}, nil); err != nil {
		return err
	}
	if err := providerExtensionHost().Store().DeleteAccount(scope.Plugin, scope.Provider, scope.Account, scope.Generation); err != nil {
		return err
	}
	Changed()
	return nil
}
