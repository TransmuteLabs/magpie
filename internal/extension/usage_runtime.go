package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type providerUsageSelectionKey struct{}

type providerUsageSelection struct {
	revision, config, enableGeneration string
}

func WithProviderUsageSelection(ctx context.Context, revision, config, enableGeneration string) context.Context {
	return context.WithValue(ctx, providerUsageSelectionKey{}, providerUsageSelection{revision, config, enableGeneration})
}

func (l *Lease) prepareUsageRead(ctx context.Context, scope sdk.Scope, params any) (any, *UsageStamp, error) {
	entry := l.worker.entry
	if selected, bound := ctx.Value(providerUsageSelectionKey{}).(providerUsageSelection); bound &&
		(selected.revision != entry.Revision || selected.config != l.ConfigVersion() ||
			selected.enableGeneration != entry.EnableGeneration) {
		return nil, nil, storeError("usage_stale", "The selected usage module generation changed.")
	}
	manifest := entry.Manifest
	if manifest.Contract.Version.Minor < 4 || scope.Account == "" {
		return params, nil, nil
	}
	raw, err := json.Marshal(params)
	var request sdk.UsageRequest
	if err != nil || !decodeHostParams(raw, &request) || len(request.Credentials) != 0 && string(request.Credentials) != "null" {
		return nil, nil, errors.New("usage credentials must come from the selected account")
	}
	stamp, err := l.manager.store.UsageStampContext(ctx, scope)
	if err != nil {
		return nil, nil, err
	}
	if request.Revision != "" && request.Revision != stamp.Version() {
		return nil, nil, storeError("usage_stale", "Usage revision changed before dispatch.")
	}
	request.Revision = stamp.Version()
	return request, &stamp, nil
}

func (l *Lease) finishUsageRead(ctx context.Context, scope sdk.Scope, stamp *UsageStamp, raw json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entry := l.worker.entry
	result, fresh, err := sdk.ValidateUsageResult(entry.Manifest, raw)
	if err != nil {
		return err
	}
	if stamp == nil {
		return nil
	}
	if result.Revision != stamp.Version() {
		return storeError("usage_stale", "The usage result did not acknowledge its requested revision.")
	}
	if _, bound := SubscriptionOwnerRevision(ctx); !bound {
		ctx = WithSubscriptionOwnerRevision(ctx, "")
	}
	m := l.manager
	if fresh && stamp.Dirty {
		return m.withProviderPublication(ctx, entry, l.ConfigVersion(), scope, func(root *os.Root) error {
			_, err := acknowledgeUsageLocked(ctx, root, scope, stamp.Revision)
			return err
		})
	}
	return m.withProviderStore(ctx, entry, l.ConfigVersion(), scope, false, func(root *os.Root, snapshot *storeReadSnapshot[storeAccounts]) error {
		current, err := m.store.usageStampFromSnapshot(ctx, root, snapshot, scope, storeSync)
		if err != nil {
			return err
		}
		if current.Revision != stamp.Revision {
			return storeError("usage_stale", "The usage read preceded an invalidation.")
		}
		return nil
	})
}

func (m *Manager) invalidateUsage(ctx context.Context, scope sdk.Scope, raw json.RawMessage, authority hostAuthority) (any, error) {
	manifest := authority.manifest
	var request sdk.UsageInvalidateRequest
	if manifest.Contract.Version.Minor < 4 || !manifest.Contract.Has(sdk.UsageReporter) ||
		!manifest.Contract.Has(sdk.AccountAuth) || scope.Account == "" || scope.Operation == "" ||
		!decodeHostParams(raw, &request) {
		return nil, errors.New("usage invalidation requires SDK 1.4 and a selected account write")
	}
	readOnly, _ := ctx.Value(gatewayReadOnlyKey{}).(bool)
	component, owned := ctx.Value(componentAuthorityKey{}).(componentAuthority)
	if readOnly || !owned || component.effect != "write" || component.group == nil {
		return nil, errors.New("a read cannot invalidate usage")
	}
	stamp, err := m.store.invalidateUsage(ctx, scope, true)
	if err != nil {
		return nil, err
	}
	return sdk.UsageInvalidateResult{Revision: stamp.Version()}, nil
}

func validateCancelIntent(params any, scope sdk.Scope) error {
	raw, err := json.Marshal(params)
	var request sdk.OperationRequest
	if err != nil || !decodeHostParams(raw, &request) ||
		request.OperationID != "" && !storeToken(request.OperationID) ||
		request.OperationID == "" && (len(request.Reference) == 0 || string(request.Reference) == "null") {
		return errors.New("business cancellation requires a bounded target operation or reference")
	}
	if request.OperationID != "" && scope.Operation == request.OperationID {
		return errors.New("cancellation intent must differ from its target operation")
	}
	return nil
}
