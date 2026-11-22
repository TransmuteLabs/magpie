package extension

import (
	"context"
	"os"

	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func (m *Manager) AcquireOutsideCalls(ctx context.Context, entry Installed, configVersion string) (*Lease, error) {
	return m.acquireLease(ctx, entry.Manifest.ID, &expectedGeneration{
		packageRevision: entry.Revision, configVersion: configVersion, enableGeneration: &entry.EnableGeneration,
	})
}

func (m *Manager) PublishOutsideCalls(ctx context.Context, expected Installed, configVersion string, scope sdk.Scope, publish func() error) error {
	if scope.Account == "" || scope.Generation == 0 {
		return ErrActionChanged
	}
	if publish == nil {
		return m.withProviderPublication(ctx, expected, configVersion, scope, nil)
	}
	return m.withProviderPublication(ctx, expected, configVersion, scope, func(*os.Root) error {
		return publish()
	})
}

func (m *Manager) withProviderPublication(ctx context.Context, expected Installed, configVersion string, scope sdk.Scope, publish func(*os.Root) error) error {
	return m.withProviderStore(ctx, expected, configVersion, scope, publish != nil, func(root *os.Root, _ *storeReadSnapshot[storeAccounts]) error {
		if publish == nil {
			return nil
		}
		return publish(root)
	})
}

func (m *Manager) withProviderStore(ctx context.Context, expected Installed, configVersion string, scope sdk.Scope, write bool, use func(*os.Root, *storeReadSnapshot[storeAccounts]) error) error {
	if m == nil || m.isClosed() || expected.Manifest.ID != scope.Plugin ||
		expected.Revision == "" || configVersion == "" || !storeValidScope(scope) {
		return ErrActionChanged
	}
	if _, bound := SubscriptionOwnerRevision(ctx); !bound {
		return errSubscriptionChanged
	}
	return m.withActiveGeneration(ctx, expected, configVersion, func() error {
		if m.isClosed() {
			return ErrActionChanged
		}
		if err := m.store.checkSubscriptionView(ctx, scope); err != nil {
			return err
		}
		withStore := m.store.withReadStoreContext
		if write {
			withStore = m.store.withStoreContext
		}
		return withStore(ctx, scope.Plugin, func(root *os.Root) error {
			var snapshot *storeReadSnapshot[storeAccounts]
			if scope.Account != "" {
				off, err := settings.AccountDiscoveryOffNow()
				if err != nil {
					return err
				}
				if off {
					return storeError("account_discovery_off", "Subscription accounts are off in magpie.")
				}
				snapshot, err = storeLoadAccountSnapshot(root, scope.Plugin, m.store.accountSnapshotSlot(scope.Plugin))
				if err != nil {
					return err
				}
				if snapshot == nil {
					return storeError("stale_generation", "Extension account generation is unavailable.")
				}
				if err := checkAccountGeneration(snapshot.value, scope); err != nil {
					return err
				}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return use(root, snapshot)
		})
	})
}
