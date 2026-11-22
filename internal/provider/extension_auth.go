package provider

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionAuthContinuation struct {
	Owner        string          `json:"magpieSubscriptionOwner"`
	Scope        sdk.Scope       `json:"scope"`
	Continuation json.RawMessage `json:"continuation,omitempty"`
}

type ExtensionAuthCompletion struct {
	sdk.AuthResult
	Generation    string `json:"generation,omitempty"`
	OwnerRevision string `json:"ownerRevision,omitempty"`
	Warning       string `json:"warning,omitempty"`
}

func extensionAuthCall(ctx context.Context, host extensionProviderHost, scope sdk.Scope, method string, params, out any) (context.Context, error) {
	if settings.AccountDiscoveryOff() {
		return ctx, errAccountDiscoveryOff
	}
	entry, err := enabledExtensionModule(ctx, host, scope.Plugin)
	if err != nil {
		return ctx, err
	}
	manifest := entry.Manifest
	if !manifest.Contract.Has(sdk.AccountAuth) {
		return ctx, errors.New("extension provider does not declare account authentication")
	}
	found := false
	for _, descriptor := range manifest.Providers {
		found = found || descriptor.ID == scope.Provider
	}
	if !found {
		return ctx, errors.New("extension provider is unavailable")
	}
	if scope.Account != "" {
		account, err := host.Store().GetAccountContext(ctx, scope.Plugin, scope.Provider, scope.Account)
		if err != nil {
			return ctx, err
		}
		if account.Generation != scope.Generation {
			return ctx, errors.New("extension account generation changed")
		}
	}
	ctx, err = extensionNetworkContext(ctx, scope)
	if err != nil {
		return ctx, err
	}
	if method == sdk.MethodAuthLogout {
		revision, _ := extensionhost.SubscriptionOwnerRevision(ctx)
		owner, ok := host.(interface {
			CheckSubscriptionScope(context.Context, sdk.Scope, string) error
		})
		if !ok {
			return ctx, errors.New("extension host does not support transactional sign-out")
		}
		if err := owner.CheckSubscriptionScope(ctx, scope, revision); err != nil {
			return ctx, err
		}
	}
	raw, err := host.Call(ctx, scope.Plugin, method, scope, params, nil)
	if err != nil {
		return ctx, err
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return ctx, errors.New("extension provider returned an invalid authentication result")
	}
	return ctx, nil
}

func StartExtensionAuth(ctx context.Context, scope sdk.Scope, request sdk.AuthStartRequest) (sdk.AuthStartResult, error) {
	var result sdk.AuthStartResult
	if request.Method == sdk.SavedSignInMethod {
		return result, errors.New("saved sign-ins require the local import command")
	}
	ctx, err := extensionAuthCall(ctx, providerExtensionHost(), scope, sdk.MethodAuthStart, request, &result)
	if err != nil {
		return sdk.AuthStartResult{}, err
	}
	if revision, _ := extensionhost.SubscriptionOwnerRevision(ctx); revision != "" {
		result.Continuation, err = json.Marshal(extensionAuthContinuation{
			Owner: revision, Scope: scope, Continuation: result.Continuation,
		})
	}
	return result, err
}

func FinishExtensionAuth(ctx context.Context, scope sdk.Scope, request sdk.AuthFinishRequest) (ExtensionAuthCompletion, error) {
	var result sdk.AuthResult
	var revision string
	var continuation extensionAuthContinuation
	if json.Unmarshal(request.Continuation, &continuation) == nil && continuation.Owner != "" {
		if continuation.Scope != scope {
			return ExtensionAuthCompletion{}, errors.New("the SDK sign-in continuation belongs to another account selection")
		}
		request.Continuation = continuation.Continuation
		revision = continuation.Owner
	}
	if expected, bound := extensionhost.SubscriptionOwnerRevision(ctx); bound && expected != revision {
		return ExtensionAuthCompletion{}, errSubscriptionOwnerChanged
	}
	ctx = extensionhost.WithSubscriptionOwnerRevision(ctx, revision)
	host := providerExtensionHost()
	ctx, err := extensionAuthCall(ctx, host, scope, sdk.MethodAuthFinish, request, &result)
	if err != nil {
		return ExtensionAuthCompletion{}, err
	}
	return saveExtensionAuth(ctx, host, scope, result)
}

func RefreshExtensionAuth(ctx context.Context, scope sdk.Scope) (sdk.AuthResult, error) {
	if scope.Account == "" || scope.Generation == 0 {
		return sdk.AuthResult{}, errors.New("an explicit extension account generation is required")
	}
	if settings.AccountDiscoveryOff() {
		return sdk.AuthResult{}, errAccountDiscoveryOff
	}
	host := providerExtensionHost()
	entry, err := enabledExtensionModule(ctx, host, scope.Plugin)
	if err != nil {
		return sdk.AuthResult{}, err
	}
	manifest := entry.Manifest
	found := false
	for _, provider := range manifest.Providers {
		found = found || provider.ID == scope.Provider
	}
	if manifest.ID != scope.Plugin || !manifest.Contract.Has(sdk.AccountAuth) || !found {
		return sdk.AuthResult{}, errors.New("extension provider authentication is unavailable")
	}
	owner, ok := host.(interface {
		RefreshAccount(context.Context, string, sdk.Scope) (sdk.AuthResult, error)
	})
	if !ok {
		return sdk.AuthResult{}, errors.New("extension host does not support transactional credential refresh")
	}
	ctx, err = extensionNetworkContext(ctx, scope)
	if err != nil {
		return sdk.AuthResult{}, err
	}
	result, err := owner.RefreshAccount(ctx, scope.Plugin, scope)
	result.Credentials = nil
	if err == nil {
		Changed()
	}
	return result, err
}

func saveExtensionAuth(ctx context.Context, host extensionProviderHost, scope sdk.Scope, result sdk.AuthResult) (ExtensionAuthCompletion, error) {
	if result.Pending {
		result.Credentials = nil
		return ExtensionAuthCompletion{AuthResult: result}, nil
	}
	id := result.ID
	if id == "" {
		id = scope.Account
	}
	if id == "" || scope.Account != "" && scope.Account != id {
		return ExtensionAuthCompletion{}, errors.New("extension authentication changed the selected account")
	}
	if !json.Valid(result.Credentials) || string(result.Credentials) == "null" {
		return ExtensionAuthCompletion{}, errors.New("extension authentication returned no credentials")
	}
	owner, ok := host.(interface {
		SaveSubscriptionAccount(context.Context, sdk.Scope, extensionhost.Account) (extensionhost.AccountGeneration, error)
	})
	if !ok {
		return ExtensionAuthCompletion{}, errors.New("extension host does not support transactional sign-in")
	}
	committed, err := owner.SaveSubscriptionAccount(ctx, scope, extensionhost.Account{
		Plugin: scope.Plugin, Provider: scope.Provider, ID: id, Name: result.Name, Auth: result.Credentials, ExpiresAt: result.ExpiresAt,
	})
	if err != nil {
		return ExtensionAuthCompletion{}, err
	}
	scope.Account, scope.Generation = committed.ID, committed.Generation
	result.ID, result.Credentials = committed.ID, nil
	completion := ExtensionAuthCompletion{AuthResult: result}
	if err := registerExtensionLogin(ctx, host, scope); err != nil {
		log.Printf("extension login policy registration failed after sign-in: %v", err)
		completion.Generation = strconv.FormatUint(committed.Generation, 10)
		completion.OwnerRevision, _ = extensionhost.SubscriptionOwnerRevision(ctx)
		completion.Warning = "The account is active. Use auth register with this account, generation and owner revision to finish local setup."
	}
	Changed()
	return completion, nil
}

func LogoutExtensionAuth(ctx context.Context, scope sdk.Scope) error {
	if scope.Account == "" || scope.Generation == 0 {
		return errors.New("an explicit extension account generation is required")
	}
	host := providerExtensionHost()
	owner, ok := host.(interface {
		DeleteSubscriptionAccount(context.Context, sdk.Scope, string) error
	})
	if !ok {
		return errors.New("extension host does not support transactional sign-out")
	}
	ctx, err := extensionAuthCall(ctx, host, scope, sdk.MethodAuthLogout, sdk.AuthLogoutRequest{}, nil)
	if err != nil {
		return err
	}
	revision, _ := extensionhost.SubscriptionOwnerRevision(ctx)
	if err := owner.DeleteSubscriptionAccount(ctx, scope, revision); err != nil {
		return err
	}
	Changed()
	return nil
}
