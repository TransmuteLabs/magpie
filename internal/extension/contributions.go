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
	if m == nil || m.isClosed() {
		return nil, errContributionRevoked
	}
	entries, err := m.catalog.list(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(entries, func(entry Installed) bool { return !activeRoot(entry) }), nil
}

func activeRoot(entry Installed) bool {
	return !entry.Disabled && entry.Manifest.Component == nil && entry.observerEnablementErr == nil
}

func (m *Manager) ActiveModule(ctx context.Context, id string) (Installed, error) {
	entries, err := m.readContributionEntries(ctx)
	if err != nil {
		return Installed{}, err
	}
	for _, entry := range entries {
		if entry.Manifest.ID == id {
			return entry, nil
		}
	}
	return Installed{}, errContributionRevoked
}

func registeredContributions[T comparable](ctx context.Context, m *Manager, module string, declared func(sdk.Manifest) []T) []contributionBinding[T] {
	var bindings []contributionBinding[T]
	for _, entry := range m.contributionEntries(ctx) {
		if module != "" && entry.Manifest.ID != module {
			continue
		}
		descriptors := declared(entry.Manifest)
		if len(descriptors) == 0 {
			continue
		}
		config, err := m.runtimeModuleConfigContext(ctx, entry.Manifest)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			continue
		}
		for _, descriptor := range descriptors {
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
