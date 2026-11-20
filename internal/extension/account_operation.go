package extension

import (
	"context"
	"errors"
	"slices"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func (lease *Lease) Namespace() string { return lease.manager.Namespace() }

func (lease *Lease) CheckCurrent(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lease.closed.Load() || lease.ctx.Err() != nil {
		return errors.New("native account operation lease closed")
	}
	return lease.manager.checkActiveGeneration(ctx, lease.worker.entry, lease.ConfigVersion())
}

func (m *Manager) checkActiveGeneration(ctx context.Context, expected Installed, configVersion string) error {
	installed, err := m.ListContext(ctx)
	if err != nil {
		return err
	}
	position := slices.IndexFunc(installed, func(entry Installed) bool {
		return entry.Manifest.ID == expected.Manifest.ID && !entry.Disabled && entry.Manifest.Component == nil &&
			entry.observerEnablementErr == nil && entry.Revision == expected.Revision &&
			entry.Directory == expected.Directory && entry.EnableGeneration == expected.EnableGeneration
	})
	if position < 0 {
		return errors.New("extension package generation changed")
	}
	config, err := m.runtimeModuleConfigContext(ctx, installed[position].Manifest)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || config.Version != configVersion {
		return errors.New("extension configuration changed")
	}
	return nil
}

func AccountOperationLease(ctx context.Context) (*Lease, error) {
	lease := inboundLeaseFrom(ctx)
	if lease == nil || lease.closed.Load() || lease.ctx.Err() != nil ||
		lease.Manifest().Contract.Version.Minor < 6 || !lease.Manifest().Contract.Has(sdk.NativeAccountOperations) {
		return nil, errors.New("native account operation has no original owned lease")
	}
	return lease, nil
}
