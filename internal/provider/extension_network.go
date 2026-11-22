package provider

import (
	"context"
	"errors"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func ExtensionNetworkContext(ctx context.Context, scope sdk.Scope) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return ctx, err
	}
	if err := netproxy.Held(ctx); err != nil {
		return ctx, err
	}
	host := providerExtensionHost()
	entry, err := enabledExtensionModule(ctx, host, scope.Plugin)
	if err != nil {
		return ctx, err
	}
	manifest := entry.Manifest
	if manifest.ID != scope.Plugin {
		return ctx, errors.New("extension network scope belongs to another module")
	}
	if scope.Provider == "" {
		if scope.Account != "" || scope.Generation != 0 {
			return ctx, errors.New("extension account scope requires a provider")
		}
		return ctx, nil
	}
	found := false
	for _, descriptor := range manifest.Providers {
		found = found || descriptor.ID == scope.Provider
	}
	if !found {
		return ctx, errors.New("extension network provider was not declared")
	}
	if scope.Account == "" && scope.Generation != 0 || scope.Account != "" && scope.Generation == 0 {
		return ctx, errors.New("extension account generation is required")
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
	return extensionNetworkContext(ctx, scope)
}

// extensionNetworkContext keeps a module's auxiliary calls under the
// selected provider/account's policy and the parent request's network hold.
func extensionNetworkContext(ctx context.Context, scope sdk.Scope) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return ctx, err
	}
	if err := netproxy.Held(ctx); err != nil {
		return ctx, err
	}
	identity, err := readSubscriptionIdentity(ctx, scope)
	if err != nil {
		return ctx, err
	}
	expected, bound := extensionhost.SubscriptionOwnerRevision(ctx)
	if !identity.visible || bound && identity.revision != expected {
		return ctx, errSubscriptionOwnerChanged
	}
	if !bound && identity.revision != "" {
		ctx = extensionhost.WithSubscriptionOwnerRevision(ctx, identity.revision)
	}
	return ViaLogin(ctx, identity.provider, identity.user), nil
}
