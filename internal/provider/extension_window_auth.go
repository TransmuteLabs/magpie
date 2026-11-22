package provider

import (
	"context"
	"log"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func ExtensionWindowAuthContext(ctx context.Context) context.Context {
	ctx = extensionhost.WithWindowAuthPolicy(ctx, func(ctx context.Context, scope sdk.Scope) (context.Context, error) {
		if settings.AccountDiscoveryOff() {
			return ctx, errAccountDiscoveryOff
		}
		return extensionNetworkContext(ctx, scope)
	})
	return extensionhost.WithWindowAuthAccountPolicy(ctx, func(ctx context.Context, host *extensionhost.Manager, scope sdk.Scope, receipt sdk.AccountSummary) {
		if err := registerExtensionAccountReceipt(ctx, host, scope, receipt); err != nil {
			log.Printf("extension login policy registration failed after sign-in: %v", err)
		}
	})
}
