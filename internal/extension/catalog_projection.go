package extension

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type CatalogProjectionReader func(context.Context, sdk.Scope, sdk.CatalogAccessDescriptor) (sdk.CatalogReadResult, error)

var catalogProjectionOwner struct {
	sync.RWMutex
	read CatalogProjectionReader
}

func SetCatalogProjectionReader(read CatalogProjectionReader) {
	catalogProjectionOwner.Lock()
	catalogProjectionOwner.read = read
	catalogProjectionOwner.Unlock()
}

func (manager *Manager) catalogProjectionCurrent(ctx context.Context, scope sdk.Scope, group *componentGroup) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if group == nil || group.ctx.Err() != nil || manager.isClosed() {
		return errors.New("catalog projection caller is no longer active")
	}
	if _, err := manager.authForContext(ctx, scope); err != nil {
		return err
	}
	return manager.checkActiveGeneration(ctx, group.entry, group.config.Version)
}

func (manager *Manager) readCatalogProjection(ctx context.Context, scope sdk.Scope, raw json.RawMessage, authority hostAuthority) (any, error) {
	manifest := authority.manifest
	var request sdk.CatalogReadRequest
	if manifest.CatalogAccess == nil || sdk.ValidateCatalogAccess(manifest) != nil ||
		!decodeHostParams(raw, &request) {
		return nil, errors.New("module did not declare this catalog read")
	}
	if manifest.CatalogAccess.Scope == "selected" && (scope.Provider == "" || scope.Account == "" || scope.Generation == 0) {
		return nil, errors.New("catalog read requires its selected account generation")
	}
	component, owned := ctx.Value(componentAuthorityKey{}).(componentAuthority)
	if !owned || manager.catalogProjectionCurrent(ctx, scope, component.group) != nil {
		return nil, errors.New("catalog read requires its current module generation")
	}
	reader := manager.catalogReader
	if reader == nil {
		catalogProjectionOwner.RLock()
		reader = catalogProjectionOwner.read
		catalogProjectionOwner.RUnlock()
	}
	if reader == nil {
		return nil, errors.New("catalog projection owner is unavailable")
	}
	result, err := reader(ctx, scope, *manifest.CatalogAccess)
	if err != nil {
		return nil, errors.New("catalog projection is unavailable")
	}
	if sdk.ValidateCatalogProjection(result) != nil {
		return nil, errors.New("catalog projection owner returned invalid evidence")
	}
	if err := manager.catalogProjectionCurrent(ctx, scope, component.group); err != nil {
		return nil, err
	}
	return result, nil
}
