package extension

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type ModuleSettingsRevision struct {
	PackageRevision  string `json:"packageRevision"`
	ConfigVersion    string `json:"configVersion"`
	EnableGeneration string `json:"enableGeneration"`
}

type ModuleSettingsSnapshot struct {
	ModuleSettingsRevision
	Values map[string]json.RawMessage `json:"values"`
}

type ModuleSettingsResult struct {
	Snapshot ModuleSettingsSnapshot
	Err      error
}

func (m *Manager) ListWithSettings(ctx context.Context) ([]Installed, map[string]ModuleSettingsResult, error) {
	var entries []Installed
	options := make(map[string]ModuleSettingsResult)
	err := m.catalog.withView(ctx, func(view catalogView) error {
		entries = make([]Installed, 0, len(view.entries))
		for _, entry := range view.entries {
			entry = view.enabledEntry(entry)
			entries = append(entries, cloneInstalled(entry))
			if err := sdk.CheckCompatibility(entry.Manifest); err != nil {
				options[entry.Manifest.ID] = ModuleSettingsResult{Err: err}
				continue
			}
			if entry.Disabled || len(entry.Manifest.Settings) == 0 {
				continue
			}
			result := ModuleSettingsResult{Err: m.moduleSettingsOwner(entry)}
			if result.Err == nil {
				result.Snapshot, result.Err = m.readModuleSettings(ctx, entry)
			}
			options[entry.Manifest.ID] = result
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return entries, options, nil
}

func (m *Manager) moduleSettingsOwner(entry Installed) error {
	if m == nil || m.isClosed() {
		return errModuleConfigClosed
	}
	if entry.Disabled || entry.Manifest.Component != nil || len(entry.Manifest.Settings) == 0 || entry.Revision == "" {
		return errors.New("settings require an enabled root module with declared settings")
	}
	return entry.observerEnablementErr
}

func (m *Manager) ModuleSettings(ctx context.Context, entry Installed) (ModuleSettingsSnapshot, error) {
	if err := m.moduleSettingsOwner(entry); err != nil {
		return ModuleSettingsSnapshot{}, err
	}
	var snapshot ModuleSettingsSnapshot
	err := m.catalog.withView(ctx, func(view catalogView) error {
		if m.isClosed() {
			return errModuleConfigClosed
		}
		current, err := view.configEntry(entry.Manifest.ID, expectedGeneration{
			packageRevision: entry.Revision, enableGeneration: &entry.EnableGeneration,
		})
		if err != nil {
			return err
		}
		current.EnableGeneration = entry.EnableGeneration
		snapshot, err = m.readModuleSettings(ctx, current)
		return err
	})
	if err != nil {
		return ModuleSettingsSnapshot{}, err
	}
	return snapshot, nil
}

func (m *Manager) readModuleSettings(ctx context.Context, entry Installed) (ModuleSettingsSnapshot, error) {
	config, err := m.runtimeModuleConfigContext(ctx, entry)
	if err != nil {
		return ModuleSettingsSnapshot{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(config.Values, &fields); err != nil {
		return ModuleSettingsSnapshot{}, err
	}
	values := make(map[string]json.RawMessage, len(entry.Manifest.Settings))
	for _, setting := range entry.Manifest.Settings {
		values[setting.Name] = fields[setting.Name]
	}
	return ModuleSettingsSnapshot{ModuleSettingsRevision: ModuleSettingsRevision{
		PackageRevision: entry.Revision, ConfigVersion: config.Version, EnableGeneration: entry.EnableGeneration,
	}, Values: values}, nil
}

func (m *Manager) CompareAndSwapModuleSettings(ctx context.Context, id string, revision ModuleSettingsRevision, changes json.RawMessage) error {
	if revision.PackageRevision == "" {
		return ErrCatalogConflict
	}
	_, err := m.updateModuleConfig(ctx, id, "", expectedGeneration{
		packageRevision: revision.PackageRevision, configVersion: revision.ConfigVersion,
		enableGeneration: &revision.EnableGeneration,
	}, changes)
	return err
}

func (m *Manager) patchModuleSettings(ctx context.Context, manifest sdk.Manifest, expected string, values json.RawMessage) (json.RawMessage, error) {
	var changes map[string]json.RawMessage
	if err := json.Unmarshal(values, &changes); err != nil {
		return nil, err
	}
	if len(changes) == 0 || len(changes) > len(manifest.Settings) {
		return nil, errors.New("module settings require a non-empty update of declared keys")
	}
	config, err := m.readModuleConfigContext(ctx, manifest.ID)
	if err != nil {
		return nil, err
	}
	if config.Version != expected {
		return nil, errors.New("module settings configuration changed")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(config.Values, &fields); err != nil {
		return nil, err
	}
	for key, value := range changes {
		if !slices.ContainsFunc(manifest.Settings, func(setting sdk.FeatureSetting) bool { return setting.Name == key }) {
			return nil, errors.New("module did not declare this setting")
		}
		fields[key] = value
	}
	return json.Marshal(fields)
}
