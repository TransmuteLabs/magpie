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
	entries, err := host.List()
	if err != nil {
		return fail(err)
	}
	index := slices.IndexFunc(entries, func(entry extensionhost.Installed) bool {
		return entry.Manifest.ID == scope.Plugin && !entry.Disabled && entry.Manifest.Contract.Has(sdk.AccountAuth) &&
			slices.ContainsFunc(entry.Manifest.Providers, func(descriptor sdk.ProviderDescriptor) bool { return descriptor.ID == scope.Provider })
	})
	if index < 0 {
		return fail(errors.New("extension account provider is unavailable"))
	}
	entry := entries[index]
	config := sdk.ConfigSnapshot{Version: "0"}
	if entry.Manifest.Contract.Version.Minor >= 1 {
		config, err = host.ModuleConfig(ctx, scope.Plugin)
		if err != nil {
			return fail(err)
		}
	}
	choice, networkSelected := "", false
	check := func(current context.Context) error {
		if err := current.Err(); err != nil {
			return err
		}
		if err := netproxy.Held(current); err != nil {
			return err
		}
		policy, err := extensionKeepAlivePolicyNow()
		if err != nil {
			return err
		}
		id := ExtensionProviderID(scope.Plugin, scope.Provider)
		selected := extensionSignIn{host: host, entry: entry, config: config.Version,
			scope: extensionhost.AccountPublicationScope(current, host.Namespace(), scope)}
		for _, descriptor := range policy.providers.Providers {
			if descriptor.ID == id && (descriptor.Off || descriptor.Hidden) {
				return errors.New("extension account provider is off")
			}
		}
		for _, login := range policy.logins {
			if login.Agent == id && login.User == scope.Account && !policy.allowsSelection(selected, selected.scope.Generation) {
				return errors.New("extension account is off, paused or unavailable")
			}
		}
		entries, err := host.List()
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(entries, func(now extensionhost.Installed) bool {
			return now.Manifest.ID == scope.Plugin && !now.Disabled && now.Revision == entry.Revision
		}) {
			return errors.New("extension tool package changed")
		}
		if entry.Manifest.Contract.Version.Minor >= 1 {
			now, err := host.ModuleConfig(current, scope.Plugin)
			if err != nil {
				return err
			}
			if now.Version != config.Version {
				return errors.New("extension tool configuration changed")
			}
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
	account, err := host.Store().RefreshedAccount(scope)
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
		account, err = host.Store().RefreshedAccount(scope)
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
		account, err := host.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
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
	return extensionhost.WithToolAccountCheck(ctx, host.Namespace(), entry.Revision, config.Version, admit), scope, nil
}
