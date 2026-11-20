package extension

import (
	"context"
	"errors"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func (m *Manager) ActiveModuleConfig(ctx context.Context, id string) (Installed, sdk.ConfigSnapshot, error) {
	if ctx == nil || m == nil || m.catalog == nil || m.store == nil || !sdk.ValidID(id) {
		return Installed{}, sdk.ConfigSnapshot{}, ErrActionChanged
	}
	if err := ctx.Err(); err != nil {
		return Installed{}, sdk.ConfigSnapshot{}, err
	}
	if m.isClosed() {
		return Installed{}, sdk.ConfigSnapshot{}, errModuleConfigClosed
	}
	root, err := m.catalog.open(false)
	if err != nil {
		return Installed{}, sdk.ConfigSnapshot{}, err
	}
	defer root.Close()
	unlock, err := catalogLock(ctx, root)
	if err != nil {
		return Installed{}, sdk.ConfigSnapshot{}, err
	}
	defer unlock()
	entries, _, err := catalogRead(root)
	if err != nil {
		return Installed{}, sdk.ConfigSnapshot{}, err
	}
	for _, entry := range entries {
		if entry.Manifest.ID != id || entry.Disabled || entry.Manifest.Component != nil {
			continue
		}
		if entry.Manifest.Contract.Version.Minor < 1 {
			return Installed{}, sdk.ConfigSnapshot{}, errors.New("module configuration requires SDK API 1.1 or newer")
		}
		if tracksEnablement(entry.Manifest) {
			entry.EnableGeneration, err = readObserverEnablement(root, id)
			if err != nil {
				return Installed{}, sdk.ConfigSnapshot{}, err
			}
		}
		config, err := m.readModuleConfigContext(ctx, id)
		if err != nil {
			return Installed{}, sdk.ConfigSnapshot{}, err
		}
		if err := ctx.Err(); err != nil {
			return Installed{}, sdk.ConfigSnapshot{}, err
		}
		if m.isClosed() {
			return Installed{}, sdk.ConfigSnapshot{}, errModuleConfigClosed
		}
		return entry, config, nil
	}
	return Installed{}, sdk.ConfigSnapshot{}, ErrActionChanged
}
