package provider

import (
	"context"
	"errors"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func (p Provider) NativeAccountScope(ctx context.Context, lease *extensionhost.Lease, owner string) (context.Context, sdk.Scope, error) {
	if !p.IsExtension() || lease == nil {
		return ctx, sdk.Scope{}, errors.New("native account operation needs an installed provider")
	}
	binding := p.Account.extension
	if binding.scope.Plugin != lease.Manifest().ID || binding.scope.Provider != owner ||
		binding.host.Namespace() != lease.Namespace() {
		return ctx, sdk.Scope{}, errors.New("native account operation cannot leave its original module")
	}
	if err := binding.check(ctx); err != nil {
		return ctx, sdk.Scope{}, err
	}
	scope := binding.currentScope()
	if scope.Account == "" || scope.Generation == 0 {
		return ctx, sdk.Scope{}, errors.New("native account operation requires its selected account")
	}
	current, err := binding.host.Store().GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil || current.Generation != scope.Generation {
		return ctx, sdk.Scope{}, errors.New("native account generation changed")
	}
	ctx, err = extensionNetworkContext(ctx, scope)
	if err == nil {
		err = netproxy.Held(ctx)
	}
	return ctx, scope, err
}
