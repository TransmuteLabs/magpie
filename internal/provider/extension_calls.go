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
	scope    sdk.Scope
	provider string
	host     extensionProviderHost
	entry    extensionhost.Installed
	config   string
}

func (account ExtensionOutsideAccount) Scope() sdk.Scope { return account.scope }

func (account ExtensionOutsideAccount) ProviderID() string { return account.provider }

func ExtensionOutsideAccounts() ([]ExtensionOutsideAccount, error) {
	if settings.AccountDiscoveryOff() {
		return nil, nil
	}
	host := providerExtensionHost()
	entries, err := host.List()
	if err != nil {
		return nil, err
	}
	var accounts []ExtensionOutsideAccount
	for _, entry := range entries {
		if entry.Disabled || entry.Manifest.Contract.Version.Minor < 8 {
			continue
		}
		for _, descriptor := range entry.Manifest.Providers {
			if !descriptor.Calls {
				continue
			}
			config, err := host.ModuleConfig(context.Background(), entry.Manifest.ID)
			if err != nil {
				return nil, err
			}
			selected, err := host.Store().Accounts(entry.Manifest.ID, descriptor.ID)
			if err != nil {
				return nil, err
			}
			for _, account := range selected {
				accounts = append(accounts, ExtensionOutsideAccount{
					scope: sdk.Scope{Plugin: entry.Manifest.ID, Provider: descriptor.ID,
						Account: account.ID, Generation: account.Generation},
					provider: ExtensionProviderID(entry.Manifest.ID, descriptor.ID),
					host:     host, entry: entry, config: config.Version,
				})
			}
		}
	}
	return accounts, nil
}

func (account ExtensionOutsideAccount) Current(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if settings.AccountDiscoveryOff() || account.host == nil {
		return errors.New("outside account discovery is unavailable")
	}
	entries, err := account.host.List()
	if err != nil || !slices.ContainsFunc(entries, func(entry extensionhost.Installed) bool {
		return entry.Manifest.ID == account.scope.Plugin && !entry.Disabled && entry.Revision == account.entry.Revision
	}) {
		return extensionhost.ErrActionChanged
	}
	config, err := account.host.ModuleConfig(ctx, account.scope.Plugin)
	if err != nil || config.Version != account.config {
		return extensionhost.ErrActionChanged
	}
	selected, err := account.host.Store().GetAccount(account.scope.Plugin, account.scope.Provider, account.scope.Account)
	if err != nil || selected.Generation != account.scope.Generation {
		return errors.New("outside account generation changed")
	}
	return nil
}

func (account ExtensionOutsideAccount) Calls(ctx context.Context, since, until time.Time) ([]sdk.OutsideCall, error) {
	if err := account.Current(ctx); err != nil {
		return nil, err
	}
	host, supported := account.host.(interface {
		AcquireRevision(context.Context, string, string, string) (*extensionhost.Lease, error)
	})
	if !supported {
		return nil, errors.New("outside calls require the selected runtime lease")
	}
	lease, err := host.AcquireRevision(ctx, account.scope.Plugin, account.entry.Revision, account.config)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	ctx, err = extensionNetworkContext(ctx, account.scope)
	if err != nil {
		return nil, err
	}
	request := sdk.OutsideCallsRequest{Since: since.UnixMilli(), Until: until.UnixMilli()}
	cursors := map[string]bool{}
	var calls []sdk.OutsideCall
	for range sdk.MaxOutsideCallPages {
		if err := account.Current(ctx); err != nil {
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
			if err := account.Current(ctx); err != nil {
				return nil, err
			}
			return calls, nil
		}
		request.Cursor = page.NextCursor
	}
	return nil, errors.New("outside-call window exceeds its page budget")
}
