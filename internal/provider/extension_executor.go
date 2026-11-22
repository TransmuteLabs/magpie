package provider

import (
	"context"

	"github.com/yetone/magpie/internal/extension"
)

func (provider Provider) CheckExecutor(ctx context.Context) error {
	module, namespace := "", ""
	if provider.IsExtension() {
		module = provider.Account.extension.scope.Plugin
		namespace = provider.Account.extension.host.Namespace()
		binding := provider.Account.extension
		if err := CheckExtensionSubscription(ctx, module, binding.scope.Provider, binding.identity().revision); err != nil {
			return err
		}
	}
	if provider.IsPlugin() && currentSubscriptionOwners(ctx).replaces(provider.PluginProvider()) {
		return errSubscriptionOwnerChanged
	}
	return extension.CheckExecutor(ctx, module, namespace)
}
