package extension

import (
	"context"
	"os"
	"sync"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type UsageModuleStamp struct {
	PackageRevision  string
	ConfigVersion    string
	EnableGeneration string
}

type UsageSnapshot struct {
	Owners   []SubscriptionOwner
	Modules  map[string]UsageModuleStamp
	Accounts map[sdk.Scope]UsageStamp
}

func (m *Manager) UsageSnapshot(ctx context.Context, scopes []sdk.Scope) (UsageSnapshot, error) {
	if m == nil || m.catalog == nil || m.store == nil || m.isClosed() {
		return UsageSnapshot{}, ErrActionChanged
	}
	if err := ctx.Err(); err != nil {
		return UsageSnapshot{}, err
	}
	selected := map[string]map[sdk.Scope]bool{}
	for _, scope := range scopes {
		if !storeValidScope(scope) || scope.Account == "" {
			continue
		}
		if selected[scope.Plugin] == nil {
			selected[scope.Plugin] = map[sdk.Scope]bool{}
		}
		selected[scope.Plugin][scope] = true
	}
	var result UsageSnapshot
	err := m.catalog.withView(ctx, func(view catalogView) error {
		return m.store.withReadPrivateContext(ctx, func(private *os.Root) error {
			snapshot := UsageSnapshot{
				Modules:  make(map[string]UsageModuleStamp, len(selected)),
				Accounts: make(map[sdk.Scope]UsageStamp, len(scopes)),
			}
			err := readStoreNamespace(ctx, private, savedSignInsDir, func(root *os.Root) error {
				var err error
				snapshot.Owners, err = m.store.readSubscriptionOwners(root)
				return err
			})
			if err != nil {
				return err
			}
			for module, accounts := range selected {
				if err := ctx.Err(); err != nil {
					return err
				}
				entry, found := view.lookup(module)
				if !found || !activeRoot(entry) {
					continue
				}
				entry = view.enabledEntry(entry)
				if activeRoot(entry) {
					m.store.readUsageModule(ctx, private, entry, accounts, &snapshot)
				}
			}
			result = snapshot
			return ctx.Err()
		})
	})
	if err != nil {
		return UsageSnapshot{}, err
	}
	if m.isClosed() {
		return UsageSnapshot{}, ErrActionChanged
	}
	return result, nil
}

func (s *Store) readUsageModule(ctx context.Context, private *os.Root, entry Installed, selected map[sdk.Scope]bool, out *UsageSnapshot) {
	module := entry.Manifest.ID
	_ = readStoreNamespace(ctx, private, module, func(root *os.Root) error {
		accounts, err := storeLoadAccountSnapshot(root, module, s.accountSnapshotSlot(module))
		if err != nil || accounts == nil {
			return err
		}
		version := "0"
		if entry.Manifest.Contract.Version.Minor >= 1 {
			state, err := storeReadState(root, sdk.Scope{Plugin: module}, moduleConfigKey)
			if err != nil {
				return err
			}
			config, err := moduleConfigSnapshot(state.VersionedValue)
			if err != nil {
				return err
			}
			version = config.Version
		}
		confirmed := sync.OnceValue(func() bool {
			return s.confirmAccountSnapshot(ctx, root, accounts, storeSync) == nil
		})
		for _, account := range accounts.value.Accounts {
			if err := ctx.Err(); err != nil {
				return err
			}
			stamp := usageStamp(account)
			if !account.Deleted && !stamp.Dirty && selected[stamp.Scope] && confirmed() {
				out.Accounts[stamp.Scope] = stamp
			}
		}
		out.Modules[module] = UsageModuleStamp{
			PackageRevision: entry.Revision, ConfigVersion: version, EnableGeneration: entry.EnableGeneration,
		}
		return nil
	})
}
