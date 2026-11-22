package provider

import (
	"context"
	"errors"
	"slices"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// PrepareExtensionAccountScope renews a selected tool account before business
// dispatch. Public tools do not acquire account or refresh authority.
func PrepareExtensionAccountScope(ctx context.Context, scope sdk.Scope) (context.Context, sdk.Scope, error) {
	if scope.Account == "" {
		return ctx, scope, nil
	}
	fail := func(err error) (context.Context, sdk.Scope, error) { return ctx, scope, err }
	host := providerExtensionHost()
	entry, config, err := toolAccountModule(ctx, host, scope.Plugin)
	if err != nil {
		return fail(err)
	}
	if !entry.Manifest.Contract.Has(sdk.ToolProvider) || !entry.Manifest.Contract.Has(sdk.AccountAuth) ||
		!slices.ContainsFunc(entry.Manifest.Providers, func(descriptor sdk.ProviderDescriptor) bool {
			return descriptor.ID == scope.Provider
		}) {
		return fail(errors.New("extension does not declare authentication for this provider"))
	}
	if scope.Generation == 0 {
		account, err := host.Store().GetAccountContext(ctx, scope.Plugin, scope.Provider, scope.Account)
		if err != nil {
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			return fail(errors.New("extension account is unavailable"))
		}
		scope.Provider, scope.Account, scope.Generation = account.Provider, account.ID, account.Generation
	}
	revision := entry.Revision + "\x00" + config.Version
	choice, networkSelected := "", false
	public, err := readSubscriptionIdentity(ctx, scope)
	if err != nil {
		return fail(err)
	}
	if expected, bound := extensionhost.SubscriptionOwnerRevision(ctx); bound && expected != public.revision {
		return fail(errSubscriptionOwnerChanged)
	}
	ctx = extensionhost.WithSubscriptionOwnerRevision(ctx, public.revision)
	check := func(current context.Context) error {
		if err := current.Err(); err != nil {
			return err
		}
		if err := netproxy.Held(current); err != nil {
			return err
		}
		policy, err := extensionKeepAlivePolicyNow(current)
		if err != nil {
			return err
		}
		if err := CheckExtensionSubscription(current, scope.Plugin, scope.Provider, public.revision); err != nil {
			return err
		}
		selected := extensionSignIn{host: host, entry: entry, config: config.Version,
			scope: extensionhost.AccountPublicationScope(current, host.Namespace(), scope), public: public}
		for _, descriptor := range policy.providers.Providers {
			if descriptor.ID == public.provider && (descriptor.Off || descriptor.Hidden) {
				return errors.New("extension account provider is off")
			}
		}
		for _, login := range policy.logins {
			if public.hasPolicy(login) && !policy.allowsSelection(selected, selected.scope.Generation) {
				return errors.New("extension account is off, paused or unavailable")
			}
		}
		if _, err := selectedExtensionModule(current, host, scope.Plugin, revision, entry.EnableGeneration,
			entry.Manifest.Contract.Version.Minor >= 1); err != nil {
			return err
		}
		network, err := extensionNetworkContext(current, scope)
		if err != nil {
			return err
		}
		if err := netproxy.Held(network); err != nil {
			return err
		}
		if networkSelected && netproxy.Choice(network) != choice {
			return errors.New("extension tool network policy changed")
		}
		return nil
	}
	if err := check(ctx); err != nil {
		return fail(err)
	}
	ctx, err = extensionNetworkContext(ctx, scope)
	if err != nil {
		return fail(err)
	}
	if err := netproxy.Held(ctx); err != nil {
		return fail(err)
	}
	choice, networkSelected = netproxy.Choice(ctx), true
	account, err := host.Store().RefreshedAccountContext(ctx, scope)
	if err != nil {
		return fail(err)
	}
	if account.Generation != scope.Generation && account.ExpiresAt <= time.Now().Add(30*time.Second).UnixMilli() {
		return fail(errors.New("extension refresh successor is too close to expiry"))
	}
	scope.Generation = account.Generation
	if account.ExpiresAt != 0 && account.ExpiresAt <= time.Now().Add(extensionRefreshLead).UnixMilli() {
		owner, ok := host.(interface {
			AcquireRevision(context.Context, string, string, string) (*extensionhost.Lease, error)
		})
		if !ok {
			return fail(errors.New("extension account preflight requires its selected runtime lease"))
		}
		lease, err := owner.AcquireRevision(ctx, scope.Plugin, entry.Revision, config.Version)
		if err != nil {
			return fail(err)
		}
		defer lease.Close()
		if err := check(ctx); err != nil {
			return fail(err)
		}
		if _, err := lease.Call(ctx, sdk.MethodAuthRefresh, scope, sdk.AuthRefreshRequest{}, nil); err != nil {
			return fail(err)
		}
		account, err = host.Store().RefreshedAccountContext(ctx, scope)
		if err != nil {
			return fail(err)
		}
		if scope.Generation == ^uint64(0) || account.Generation != scope.Generation+1 || account.ExpiresAt <= time.Now().Add(30*time.Second).UnixMilli() {
			return fail(errors.New("extension credential refresh did not prove a usable successor"))
		}
		scope.Generation = account.Generation
		Changed()
	}
	admit := func(current context.Context) error {
		if err := check(current); err != nil {
			return err
		}
		account, err := host.Store().GetAccountContext(current, scope.Plugin, scope.Provider, scope.Account)
		if err != nil {
			return err
		}
		if account.Generation != extensionhost.AccountPublicationScope(current, host.Namespace(), scope).Generation {
			return errors.New("extension tool account generation changed")
		}
		return nil
	}
	if err := admit(ctx); err != nil {
		return fail(err)
	}
	return extensionhost.WithToolAccountCheck(ctx, host.Namespace(), entry.Revision, config.Version, entry.EnableGeneration, admit), scope, nil
}

func toolAccountModule(ctx context.Context, host extensionProviderHost, id string) (extensionhost.Installed, sdk.ConfigSnapshot, error) {
	if registry, ok := host.(interface {
		ActiveModuleConfig(context.Context, string) (extensionhost.Installed, sdk.ConfigSnapshot, error)
	}); ok {
		return registry.ActiveModuleConfig(ctx, id)
	}
	entry, err := enabledExtensionModule(ctx, host, id)
	config := sdk.ConfigSnapshot{Version: "0"}
	if err == nil && entry.Manifest.Contract.Version.Minor >= 1 {
		config, err = host.ModuleConfig(ctx, id)
	}
	return entry, config, err
}
