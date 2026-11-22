package extension

import (
	"context"
	"errors"

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
	return m.withActiveGeneration(ctx, expected, configVersion, nil)
}

func (m *Manager) withActiveGeneration(ctx context.Context, expected Installed, configVersion string, publish func() error) error {
	if m == nil || m.isClosed() {
		return errContributionRevoked
	}
	return m.catalog.withLockedView(ctx, func(view catalogView) error {
		entry, found := view.lookup(expected.Manifest.ID)
		if !found || !activeRoot(entry) {
			return errContributionRevoked
		}
		entry = view.enabledEntry(entry)
		if !activeRoot(entry) {
			return errContributionRevoked
		}
		if entry.Revision != expected.Revision || entry.Directory != expected.Directory ||
			entry.EnableGeneration != expected.EnableGeneration {
			return errors.New("extension package generation changed")
		}
		config, err := m.runtimeModuleConfigContext(ctx, entry)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || config.Version != configVersion {
			return errors.New("extension configuration changed")
		}
		if publish != nil {
			return publish()
		}
		return nil
	})
}

func AccountOperationLease(ctx context.Context) (*Lease, error) {
	lease := inboundLeaseFrom(ctx)
	if lease == nil || lease.closed.Load() || lease.ctx.Err() != nil ||
		lease.Manifest().Contract.Version.Minor < 6 || !lease.Manifest().Contract.Has(sdk.NativeAccountOperations) {
		return nil, errors.New("native account operation has no original owned lease")
	}
	return lease, nil
}
