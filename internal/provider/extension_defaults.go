package provider

import (
	"context"
	"slices"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func (p Provider) extensionDefaults() (*int, *int) {
	if !p.IsExtension() {
		return nil, nil
	}
	binding := p.Account.extension
	ctx := context.Background()
	if binding.manifest.Contract.Version.Minor < 9 || binding.check(ctx) != nil || binding.checkModelRevision(ctx) != nil {
		return nil, nil
	}
	current, err := binding.host.Manifest(binding.scope.Plugin)
	if err != nil || current.Contract.Version.Minor < 9 {
		return nil, nil
	}
	index := slices.IndexFunc(current.Providers, func(provider sdk.ProviderDescriptor) bool {
		return provider.ID == binding.scope.Provider
	})
	if index < 0 {
		return nil, nil
	}
	descriptor := current.Providers[index]
	same := func(before, after *int) bool {
		return before == nil && after == nil || before != nil && after != nil && *before == *after
	}
	if sdk.ValidateProviderDefaults(current, descriptor) != nil ||
		!same(descriptor.MaxConcurrency, binding.descriptor.MaxConcurrency) ||
		!same(descriptor.MaxAccounts, binding.descriptor.MaxAccounts) {
		return nil, nil
	}
	return binding.descriptor.MaxConcurrency, binding.descriptor.MaxAccounts
}
