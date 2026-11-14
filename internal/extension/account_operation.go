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
	installed, err := lease.manager.List()
	if err != nil {
		return err
	}
	position := slices.IndexFunc(installed, func(entry Installed) bool {
		return entry.Manifest.ID == lease.Manifest().ID && !entry.Disabled && entry.Revision == lease.Revision()
	})
	if position < 0 {
		return errors.New("native account operation package changed")
	}
	config, err := lease.manager.ModuleConfig(ctx, lease.Manifest().ID)
	if err != nil || config.Version != lease.ConfigVersion() {
		return errors.New("native account operation configuration changed")
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
