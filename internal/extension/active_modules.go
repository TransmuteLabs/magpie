package extension

import (
	"context"
	"slices"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func (m *Manager) ActiveModules(ctx context.Context) ([]Installed, error) {
	return m.readContributionEntries(ctx)
}

type ProviderModule struct {
	Installed
	ConfigVersion   string
	ConfigError     error
	EnablementError error
}

func ProviderDiscoveryEligible(manifest sdk.Manifest, includeAccounts bool) bool {
	return (includeAccounts || !manifest.Contract.Has(sdk.AccountAuth)) &&
		(manifest.Contract.Has(sdk.OutboundExecutor) ||
			slices.ContainsFunc(manifest.Providers, sdk.ProviderDescriptor.AccountOnly))
}

func (m *Manager) ActiveProviderModules(ctx context.Context, includeAccounts bool, include func(sdk.Manifest) bool) ([]ProviderModule, error) {
	if m == nil || m.isClosed() {
		return nil, errContributionRevoked
	}
	var modules []ProviderModule
	err := m.catalog.withView(ctx, func(view catalogView) error {
		for _, entry := range view.entries {
			if !activeRoot(entry) || !ProviderDiscoveryEligible(entry.Manifest, includeAccounts) ||
				include != nil && !include(entry.Manifest) {
				continue
			}
			entry = view.enabledEntry(entry)
			if entry.observerEnablementErr != nil {
				modules = append(modules, ProviderModule{
					Installed: cloneInstalled(entry), EnablementError: entry.observerEnablementErr,
				})
				continue
			}
			config, err := m.readActiveModuleConfig(ctx, entry)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			modules = append(modules, ProviderModule{
				Installed: cloneInstalled(entry), ConfigVersion: config.Version, ConfigError: err,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return modules, nil
}
