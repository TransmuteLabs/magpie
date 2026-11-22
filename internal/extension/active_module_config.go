package extension

import (
	"context"

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
	var result Installed
	var config sdk.ConfigSnapshot
	err := m.catalog.withView(ctx, func(view catalogView) error {
		entry, found := view.lookup(id)
		if !found || !activeRoot(entry) {
			return ErrActionChanged
		}
		entry = view.enabledEntry(entry)
		if entry.observerEnablementErr != nil {
			return entry.observerEnablementErr
		}
		current := sdk.ConfigSnapshot{Version: "0", Values: []byte("{}")}
		if entry.Manifest.Contract.Version.Minor >= 1 {
			var err error
			current, err = m.readModuleConfigContext(ctx, id)
			if err != nil {
				return err
			}
		}
		result, config = cloneInstalled(entry), current
		return nil
	})
	if err != nil {
		return Installed{}, sdk.ConfigSnapshot{}, err
	}
	if m.isClosed() {
		return Installed{}, sdk.ConfigSnapshot{}, errModuleConfigClosed
	}
	return result, config, nil
}
