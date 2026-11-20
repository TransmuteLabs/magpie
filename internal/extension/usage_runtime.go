package extension

import (
	"context"
	"encoding/json"
	"errors"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func (m *Manager) prepareUsageRead(manifest sdk.Manifest, scope sdk.Scope, params any) (any, *UsageStamp, error) {
	if manifest.Contract.Version.Minor < 4 || scope.Account == "" {
		return params, nil, nil
	}
	raw, err := json.Marshal(params)
	var request sdk.UsageRequest
	if err != nil || !decodeHostParams(raw, &request) || len(request.Credentials) != 0 && string(request.Credentials) != "null" {
		return nil, nil, errors.New("usage credentials must come from the selected account")
	}
	stamp, err := m.store.UsageStamp(scope)
	if err != nil {
		return nil, nil, err
	}
	if request.Revision != "" && request.Revision != stamp.Version() {
		return nil, nil, storeError("usage_stale", "Usage revision changed before dispatch.")
	}
	request.Revision = stamp.Version()
	return request, &stamp, nil
}

func (m *Manager) finishUsageRead(manifest sdk.Manifest, scope sdk.Scope, stamp *UsageStamp, raw json.RawMessage) error {
	result, fresh, err := sdk.ValidateUsageResult(manifest, raw)
	if err != nil {
		return err
	}
	if stamp == nil {
		return nil
	}
	if result.Revision != stamp.Version() {
		return storeError("usage_stale", "The usage result did not acknowledge its requested revision.")
	}
	current, err := m.store.UsageStamp(scope)
	if err != nil {
		return err
	}
	if current.Revision != stamp.Revision {
		return storeError("usage_stale", "The usage read preceded an invalidation.")
	}
	if fresh {
		_, err = m.store.AcknowledgeUsage(scope, stamp.Revision)
	}
	return err
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
	operation, err := m.store.OperationContext(ctx, scope.Plugin, scope.Operation)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || operation.Scope != scope || operation.State != "accepted" {
		return nil, errors.New("usage invalidation requires durable write acceptance")
	}
	stamp, err := m.store.InvalidateUsage(scope)
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
