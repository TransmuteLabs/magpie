package provider

import (
	"context"
	"errors"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const extensionRefreshLead = time.Minute

func (binding *extensionAccount) prepare(ctx context.Context) (*extensionAccount, error) {
	if err := binding.check(ctx); err != nil {
		return nil, err
	}
	origin := binding
	snapshot := *binding
	snapshot.scope, snapshot.generation = binding.currentScope(), nil
	binding = &snapshot
	if binding.scope.Account == "" || !binding.manifest.Contract.Has(sdk.AccountAuth) {
		return binding, nil
	}
	account, err := binding.host.Store().GetAccount(binding.scope.Plugin, binding.scope.Provider, binding.scope.Account)
	if err != nil {
		extensionSelectionFailure(ctx, binding.scope, err)
		return nil, err
	}
	if account.Generation != binding.scope.Generation {
		err := &sdk.Error{Code: "stale_generation", Message: "Extension account generation changed."}
		extensionSelectionFailure(ctx, binding.scope, err)
		return nil, err
	}
	if account.ExpiresAt == 0 || account.ExpiresAt > time.Now().Add(extensionRefreshLead).UnixMilli() {
		return binding, nil
	}
	ctx, err = extensionNetworkContext(ctx, binding.scope)
	if err != nil {
		return nil, err
	}
	if err := netproxy.Held(ctx); err != nil {
		return nil, err
	}
	owner, ok := binding.host.(interface {
		RefreshAccount(context.Context, string, sdk.Scope) (sdk.AuthResult, error)
	})
	if !ok {
		return nil, errors.New("extension host does not support transactional credential refresh")
	}
	if _, err := owner.RefreshAccount(ctx, binding.scope.Plugin, binding.scope); err != nil {
		return nil, err
	}
	current, err := binding.host.Store().GetAccount(binding.scope.Plugin, binding.scope.Provider, binding.scope.Account)
	if err != nil {
		return nil, err
	}
	if current.Generation != binding.scope.Generation+1 {
		return nil, errors.New("refreshed extension account generation changed")
	}
	if current.ExpiresAt != 0 && current.ExpiresAt <= time.Now().Add(30*time.Second).UnixMilli() {
		return nil, errors.New("extension credential refresh returned credentials too close to expiry")
	}
	prepared := *binding
	prepared.scope.Generation = current.Generation
	if err := prepared.check(ctx); err != nil {
		return nil, err
	}
	if origin.generation != nil {
		origin.generation.CompareAndSwap(binding.scope.Generation, current.Generation)
	}
	Changed()
	return &prepared, nil
}
