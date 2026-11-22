package provider

import (
	"context"
	"errors"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const extensionRefreshLead = time.Minute

func extensionRefreshDue(expiresAt int64) bool {
	return expiresAt != 0 && expiresAt <= time.Now().Add(extensionRefreshLead).UnixMilli()
}

func (binding *extensionAccount) prepare(ctx context.Context) (*extensionAccount, error) {
	selected, err := binding.checkedSelection(ctx, binding.manifest.Contract.Version.Minor >= 9)
	if err != nil {
		return nil, err
	}
	return binding.prepareSelection(ctx, selected)
}

func (binding *extensionAccount) prepareSelection(ctx context.Context, selected extensionSelection) (*extensionAccount, error) {
	if selected.scope.Generation != binding.currentScope().Generation {
		err := &sdk.Error{Code: "stale_generation", Message: "Extension account generation changed."}
		extensionSelectionFailure(ctx, selected.scope, err)
		return nil, err
	}
	origin := binding
	snapshot := *binding
	snapshot.scope, snapshot.generation = selected.scope, nil
	binding = &snapshot
	if binding.scope.Account == "" || !binding.manifest.Contract.Has(sdk.AccountAuth) {
		return binding, nil
	}
	if !extensionRefreshDue(selected.expiresAt) {
		return binding, nil
	}
	ctx, err := extensionNetworkContext(extensionhost.WithSubscriptionOwnerRevision(ctx, binding.identity().revision), binding.scope)
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
	current, err := binding.host.Store().GetAccountContext(ctx, binding.scope.Plugin, binding.scope.Provider, binding.scope.Account)
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
	if err := prepared.checkSelected(ctx, binding.manifest.Contract.Version.Minor >= 9); err != nil {
		return nil, err
	}
	if origin.generation != nil {
		origin.generation.CompareAndSwap(binding.scope.Generation, current.Generation)
	}
	Changed()
	return &prepared, nil
}
