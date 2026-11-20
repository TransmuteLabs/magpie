package provider

import (
	"context"
	"slices"

	extensionhost "github.com/yetone/magpie/internal/extension"
)

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
