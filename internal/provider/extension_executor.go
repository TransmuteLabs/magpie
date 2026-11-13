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
	}
	return extension.CheckExecutor(ctx, module, namespace)
}
