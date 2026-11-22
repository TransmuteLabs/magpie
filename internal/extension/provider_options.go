package extension

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type ProviderOptionState struct {
	sdk.ProviderOption
	On bool `json:"on"`
}

type ProviderOptionsSnapshot struct {
	Module          string                `json:"module"`
	Provider        string                `json:"provider"`
	PackageRevision string                `json:"packageRevision"`
	ConfigVersion   string                `json:"configVersion"`
	Options         []ProviderOptionState `json:"options"`
}

func (m *Manager) providerOptionOwner(ctx context.Context, id, provider string) (Installed, sdk.ProviderDescriptor, error) {
	if ctx == nil {
		return Installed{}, sdk.ProviderDescriptor{}, errors.New("provider options require a context")
	}
	if err := ctx.Err(); err != nil {
		return Installed{}, sdk.ProviderDescriptor{}, err
	}
	entry, err := m.ActiveModule(ctx, id)
	if errors.Is(err, errContributionRevoked) {
		return Installed{}, sdk.ProviderDescriptor{}, errors.New("provider options require an enabled installed provider")
	}
	if err != nil {
		return Installed{}, sdk.ProviderDescriptor{}, err
	}
	for _, descriptor := range entry.Manifest.Providers {
		if descriptor.ID == provider {
			return entry, descriptor, nil
		}
	}
	return Installed{}, sdk.ProviderDescriptor{}, errors.New("provider options require an enabled installed provider")
}

func (m *Manager) ProviderOptions(ctx context.Context, id, provider string) (*ProviderOptionsSnapshot, error) {
	entry, descriptor, err := m.providerOptionOwner(ctx, id, provider)
	if err != nil || len(descriptor.Options) == 0 {
		return nil, err
	}
	config, err := m.ModuleConfig(ctx, id)
	if err != nil {
		return nil, err
	}
	snapshot := &ProviderOptionsSnapshot{Module: id, Provider: provider,
		PackageRevision: entry.Revision, ConfigVersion: config.Version,
		Options: make([]ProviderOptionState, 0, len(descriptor.Options))}
	for _, option := range descriptor.Options {
		on, err := option.On(config.Values)
		if err != nil {
			return nil, err
		}
		snapshot.Options = append(snapshot.Options, ProviderOptionState{ProviderOption: option, On: on})
	}
	return snapshot, nil
}

func (m *Manager) CompareAndSwapProviderOptions(ctx context.Context, id, provider, revision, expected string,
	values map[string]bool, confirmed []string) (sdk.ConfigSnapshot, error) {
	entry, descriptor, err := m.providerOptionOwner(ctx, id, provider)
	if err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	if revision == "" || entry.Revision != revision {
		return sdk.ConfigSnapshot{}, ErrCatalogConflict
	}
	if len(values) == 0 || len(values) > 32 || len(confirmed) > 32 {
		return sdk.ConfigSnapshot{}, errors.New("provider options require a bounded switch update")
	}
	config, err := m.ModuleConfig(ctx, id)
	if err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	if config.Version != expected {
		return sdk.ConfigSnapshot{}, errors.New("provider option configuration changed")
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(config.Values, &fields); err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	for key, on := range values {
		index := slices.IndexFunc(descriptor.Options, func(option sdk.ProviderOption) bool { return option.Key == key })
		if index < 0 {
			return sdk.ConfigSnapshot{}, errors.New("provider did not declare this switch")
		}
		option := descriptor.Options[index]
		previous, err := option.On(config.Values)
		if err != nil {
			return sdk.ConfigSnapshot{}, err
		}
		if on && !previous && (option.Confirm != "" || len(option.Confirms) > 0) && !slices.Contains(confirmed, key) {
			return sdk.ConfigSnapshot{}, errors.New("enabling this provider switch requires confirmation")
		}
		fields[key] = json.RawMessage("false")
		if on {
			fields[key] = json.RawMessage("true")
		}
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	return m.updateModuleConfig(ctx, id, provider, expectedGeneration{
		packageRevision: revision, configVersion: expected,
	}, body)
}
