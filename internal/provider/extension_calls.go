package provider

import (
	"context"
	"errors"
	"slices"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type ExtensionOutsideAccount struct {
	scope  sdk.Scope
	public extensionPublicIdentity
	host   *extensionhost.Manager
	entry  extensionhost.Installed
	config string
}

func (account ExtensionOutsideAccount) Scope() sdk.Scope { return account.scope }

func (account ExtensionOutsideAccount) PublicLogin() (provider, user string) {
	return account.public.provider, account.public.user
}

func ExtensionOutsideAccounts(ctx context.Context) ([]ExtensionOutsideAccount, error) {
	if settings.AccountDiscoveryOff() {
		return nil, nil
	}
	host, supported := providerExtensionHost().(*extensionhost.Manager)
	if !supported {
		return nil, errors.New("outside calls require the selected runtime")
	}
	entries, err := host.ActiveModules(ctx)
	if err != nil {
		return nil, err
	}
	owners, err := host.Store().SubscriptionOwners(ctx)
	if err != nil {
		return nil, err
	}
	public := extensionSubscriptionOwners{rows: owners}
	var accounts []ExtensionOutsideAccount
	for _, entry := range entries {
		if entry.Manifest.Contract.Version.Minor < 8 {
			continue
		}
		descriptors := slices.DeleteFunc(slices.Clone(entry.Manifest.Providers), func(descriptor sdk.ProviderDescriptor) bool {
			return !descriptor.Calls
		})
		if len(descriptors) == 0 {
			continue
		}
		config, err := host.ModuleConfig(ctx, entry.Manifest.ID)
		if err != nil {
			return nil, err
		}
		selected, err := extensionAccountMetadata(ctx, host, entry.Manifest.ID, descriptors, false)
		if err != nil {
			return nil, err
		}
		for _, descriptor := range descriptors {
			for _, account := range selected[descriptor.ID] {
				scope := sdk.Scope{Plugin: entry.Manifest.ID, Provider: descriptor.ID,
					Account: account.ID, Generation: account.Generation}
				identity := public.identity(scope)
				if !identity.visible {
					continue
				}
				accounts = append(accounts, ExtensionOutsideAccount{
					scope: scope, public: identity, host: host, entry: entry, config: config.Version,
				})
			}
		}
	}
	return accounts, nil
}

func (account ExtensionOutsideAccount) Publish(ctx context.Context, publish func() error) error {
	if settings.AccountDiscoveryOff() || account.host == nil {
		return errors.New("outside account discovery is unavailable")
	}
	ctx = extensionhost.WithSubscriptionOwnerRevision(ctx, account.public.revision)
	return account.host.PublishOutsideCalls(ctx, account.entry, account.config, account.scope, publish)
}

func (account ExtensionOutsideAccount) Calls(ctx context.Context, since, until time.Time) ([]sdk.OutsideCall, error) {
	if err := account.Publish(ctx, nil); err != nil {
		return nil, err
	}
	ctx, err := extensionNetworkContext(extensionhost.WithSubscriptionOwnerRevision(ctx, account.public.revision), account.scope)
	if err != nil {
		return nil, err
	}
	lease, err := account.host.AcquireOutsideCalls(ctx, account.entry, account.config)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	request := sdk.OutsideCallsRequest{Since: since.UnixMilli(), Until: until.UnixMilli()}
	cursors := map[string]bool{}
	var calls []sdk.OutsideCall
	for range sdk.MaxOutsideCallPages {
		if err := account.Publish(ctx, nil); err != nil {
			return nil, err
		}
		if cursors[request.Cursor] {
			return nil, errors.New("outside-call reader repeated its page cursor")
		}
		cursors[request.Cursor] = true
		raw, err := lease.Call(ctx, sdk.MethodProviderCalls, account.scope, request, nil)
		if err != nil {
			return nil, err
		}
		page, err := sdk.DecodeOutsideCallsResult(account.entry.Manifest, request, raw)
		if err != nil {
			return nil, err
		}
		if len(calls)+len(page.Calls) > 5000 {
			return nil, errors.New("outside-call window exceeds its record budget")
		}
		for _, call := range page.Calls {
			call.Usage = *call.Usage.Clone()
			calls = append(calls, call)
		}
		if page.NextCursor == "" {
			if err := account.Publish(ctx, nil); err != nil {
				return nil, err
			}
			return calls, nil
		}
		request.Cursor = page.NextCursor
	}
	return nil, errors.New("outside-call window exceeds its page budget")
}
