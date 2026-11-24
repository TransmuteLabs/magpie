package provider

import (
	"context"
	"slices"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func configuredProviderModules(ctx context.Context, host extensionProviderHost, include func(sdk.Manifest) bool) ([]extensionhost.ProviderModule, error) {
	includeAccounts := !settings.AccountDiscoveryOff()
	if registry, ok := host.(interface {
		ActiveProviderModules(context.Context, bool, func(sdk.Manifest) bool) ([]extensionhost.ProviderModule, error)
	}); ok {
		return registry.ActiveProviderModules(ctx, includeAccounts, include)
	}
	entries, err := activeProviderModules(ctx, host)
	if err != nil {
		return nil, err
	}
	var modules []extensionhost.ProviderModule
	for _, entry := range entries {
		if !extensionhost.ProviderDiscoveryEligible(entry.Manifest, includeAccounts) ||
			include != nil && !include(entry.Manifest) {
			continue
		}
		version, err := extensionConfigVersion(ctx, host, entry)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		entry.Manifest = entry.Manifest.Clone()
		modules = append(modules, extensionhost.ProviderModule{
			Installed: entry, ConfigVersion: version, ConfigError: err,
		})
	}
	return modules, nil
}

func activeProviderModules(ctx context.Context, host extensionProviderHost) ([]extensionhost.Installed, error) {
	if registry, ok := host.(interface {
		ActiveModules(context.Context) ([]extensionhost.Installed, error)
	}); ok {
		return registry.ActiveModules(ctx)
	}
	entries, err := host.ListContext(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(slices.Clone(entries), func(entry extensionhost.Installed) bool {
		return entry.Disabled || entry.Manifest.Component != nil
	}), nil
}
