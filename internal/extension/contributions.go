package extension

import (
	"context"
	"errors"
	"slices"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type contributionBinding[T comparable] struct {
	Namespace     string
	Module        string
	Revision      string
	ConfigVersion string
	Descriptor    T

	enableGeneration string
}

var (
	errContributionRevoked = errors.New("extension contribution was revoked")
	errContributionConfig  = errors.New("extension contribution configuration changed")
)

func (m *Manager) contributionEntries(ctx context.Context) []Installed {
	entries, _ := m.readContributionEntries(ctx)
	return entries
}

func (m *Manager) readContributionEntries(ctx context.Context) ([]Installed, error) {
	return m.selectContributions(ctx, nil)
}

func (m *Manager) selectContributionPoint(ctx context.Context, point sdk.ContributionPoint, version int) ([]Installed, error) {
	return m.selectContributions(ctx, func(entry Installed) bool {
		return slices.ContainsFunc(entry.Manifest.Contributions, func(contribution sdk.ContributionDescriptor) bool {
			return contribution.Point == point && contribution.Version == version
		})
	})
}

func (m *Manager) selectContributions(ctx context.Context, include func(Installed) bool) ([]Installed, error) {
	if m == nil || m.isClosed() {
		return nil, errContributionRevoked
	}
	var entries []Installed
	err := m.catalog.withView(ctx, func(view catalogView) error {
		entries = []Installed{}
		for _, entry := range view.entries {
			if !activeRoot(entry) || include != nil && !include(entry) {
				continue
			}
			if entry = view.enabledEntry(entry); activeRoot(entry) {
				entries = append(entries, cloneInstalled(entry))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func activeRoot(entry Installed) bool {
	return !entry.Disabled && entry.Manifest.Component == nil && entry.observerEnablementErr == nil
}

func (m *Manager) ActiveModule(ctx context.Context, id string) (Installed, error) {
	if m == nil || m.isClosed() {
		return Installed{}, errContributionRevoked
	}
	var result Installed
	err := m.catalog.withView(ctx, func(view catalogView) error {
		entry, found := view.lookup(id)
		if found && activeRoot(entry) {
			entry = view.enabledEntry(entry)
			if activeRoot(entry) {
				result = cloneInstalled(entry)
				return nil
			}
		}
		return errContributionRevoked
	})
	if err != nil {
		return Installed{}, err
	}
	return result, nil
}

func registeredContributions[T comparable](ctx context.Context, m *Manager, module string, declared func(sdk.Manifest) []T) []contributionBinding[T] {
	entries, _ := m.selectContributions(ctx, func(entry Installed) bool {
		return (module == "" || module == entry.Manifest.ID) && len(declared(entry.Manifest)) > 0
	})
	var bindings []contributionBinding[T]
	for _, entry := range entries {
		config, err := m.runtimeModuleConfigContext(ctx, entry)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			continue
		}
		for _, descriptor := range declared(entry.Manifest) {
			bindings = append(bindings, contributionBinding[T]{
				Namespace: m.Namespace(), Module: entry.Manifest.ID, Revision: entry.Revision,
				ConfigVersion: config.Version, Descriptor: descriptor, enableGeneration: entry.EnableGeneration,
			})
		}
	}
	return bindings
}

func contributionName(name string) (module, id string, valid bool) {
	module, id, found := strings.Cut(name, "/")
	return module, id, found && sdk.ValidID(module) && sdk.ValidID(id)
}

func checkContribution[T comparable](ctx context.Context, m *Manager, binding contributionBinding[T], declared func(sdk.Manifest) []T) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if binding.Module != "" && binding.Namespace == m.Namespace() {
		bindings := registeredContributions(ctx, m, binding.Module, declared)
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, current := range bindings {
			if current.Revision == binding.Revision && current.enableGeneration == binding.enableGeneration &&
				current.Descriptor == binding.Descriptor {
				if current.ConfigVersion != binding.ConfigVersion {
					return errContributionConfig
				}
				return nil
			}
		}
	}
	return errContributionRevoked
}

func (binding contributionBinding[T]) acquire(ctx context.Context, m *Manager) (*Lease, error) {
	return m.acquireLease(ctx, binding.Module, &expectedGeneration{
		packageRevision: binding.Revision, configVersion: binding.ConfigVersion, enableGeneration: &binding.enableGeneration,
	})
}

func withContributionAdmission(ctx context.Context, check func(context.Context) error) context.Context {
	previous, _ := ctx.Value(windowAdmissionKey{}).(func(context.Context) error)
	return context.WithValue(ctx, windowAdmissionKey{}, func(admission context.Context) error {
		if previous != nil {
			if err := previous(admission); err != nil {
				return err
			}
		}
		return check(admission)
	})
}
