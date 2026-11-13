package provider

import (
	"context"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func ExtensionWindowAuthContext(ctx context.Context) context.Context {
	return extensionhost.WithWindowAuthPolicy(ctx, func(ctx context.Context, scope sdk.Scope) (context.Context, error) {
		if settings.AccountDiscoveryOff() {
			return ctx, errAccountDiscoveryOff
		}
		return extensionNetworkContext(ctx, scope)
	})
}
