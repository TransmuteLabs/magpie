package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	moduleConfigKey           = "module.config"
	moduleConfigReloadTimeout = 10 * time.Second
)

var errModuleConfigClosed = errors.New("extension manager is closed")

func (m *Manager) ModuleConfig(ctx context.Context, id string) (sdk.ConfigSnapshot, error) {
	if ctx == nil || m == nil || m.store == nil || !sdk.ValidID(id) {
		return sdk.ConfigSnapshot{}, errors.New("module configuration requires a manager, context and module identity")
	}
	if m.isClosed() {
		return sdk.ConfigSnapshot{}, errModuleConfigClosed
	}
	var config sdk.ConfigSnapshot
	err := m.catalog.withView(ctx, func(view catalogView) error {
		if _, err := view.configEntry(id, expectedGeneration{}); err != nil {
			return err
		}
		var err error
		config, err = m.readModuleConfigContext(ctx, id)
		return err
	})
	return config, err
}

func (m *Manager) readModuleConfig(id string) (sdk.ConfigSnapshot, error) {
	return m.readModuleConfigContext(context.Background(), id)
}

func (m *Manager) readModuleConfigContext(ctx context.Context, id string) (sdk.ConfigSnapshot, error) {
	value, err := m.readModuleConfigValue(ctx, id)
	if err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	return moduleConfigSnapshot(value)
}

func (m *Manager) readModuleConfigValue(ctx context.Context, id string) (VersionedValue, error) {
	return m.store.StateGetContext(ctx, sdk.Scope{Plugin: id}, moduleConfigKey)
}

func moduleConfigSnapshot(value VersionedValue) (sdk.ConfigSnapshot, error) {
	if value.Version == 0 {
		return sdk.ConfigSnapshot{Version: "0", Values: json.RawMessage(`{}`)}, nil
	}
	if err := moduleConfigValues(value.Value); err != nil {
		return sdk.ConfigSnapshot{}, errors.New("module configuration is unreadable")
	}
	return sdk.ConfigSnapshot{Version: strconv.FormatUint(value.Version, 10), Values: bytes.Clone(value.Value)}, nil
}

func (m *Manager) CompareAndSwapModuleConfig(ctx context.Context, id, expected string, values json.RawMessage) (sdk.ConfigSnapshot, error) {
	return m.compareAndSwapModuleConfig(ctx, id, "", expected, values)
}

func (m *Manager) compareAndSwapModuleConfig(ctx context.Context, id, packageRevision, expected string, values json.RawMessage) (sdk.ConfigSnapshot, error) {
	return m.updateModuleConfig(ctx, id, "", expectedGeneration{packageRevision: packageRevision, configVersion: expected}, values)
}

func (m *Manager) updateModuleConfig(ctx context.Context, id, provider string, expected expectedGeneration, values json.RawMessage) (sdk.ConfigSnapshot, error) {
	if ctx == nil || m == nil || m.catalog == nil || m.store == nil || !sdk.ValidID(id) {
		return sdk.ConfigSnapshot{}, errors.New("module configuration requires its installed root owner")
	}
	if m.isClosed() {
		return sdk.ConfigSnapshot{}, errModuleConfigClosed
	}
	version, err := strconv.ParseUint(expected.configVersion, 10, 64)
	if err != nil || strconv.FormatUint(version, 10) != expected.configVersion {
		return sdk.ConfigSnapshot{}, errors.New("module configuration requires a canonical CAS version")
	}
	if err := moduleConfigValues(values); err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	updated, err := func() (VersionedValue, error) {
		root, err := m.catalog.open(false)
		if err != nil {
			return VersionedValue{}, err
		}
		defer root.Close()
		unlock, err := catalogLock(ctx, root)
		if err != nil {
			return VersionedValue{}, err
		}
		defer unlock()
		view, err := m.catalog.readView(root)
		if err != nil {
			return VersionedValue{}, err
		}
		entry, err := view.configEntry(id, expected)
		if err != nil {
			return VersionedValue{}, err
		}
		if _, bound := SubscriptionOwnerRevision(ctx); provider != "" && bound {
			if err := m.store.checkSubscriptionView(ctx, sdk.Scope{Plugin: id, Provider: provider}); err != nil {
				return VersionedValue{}, err
			}
		}
		config := values
		if expected.enableGeneration != nil {
			config, err = m.patchModuleSettings(ctx, entry.Manifest, expected.configVersion, values)
			if err != nil {
				return VersionedValue{}, err
			}
		}
		if _, err := effectiveModuleConfig(entry.Manifest, config); err != nil {
			return VersionedValue{}, err
		}
		if err := ctx.Err(); err != nil {
			return VersionedValue{}, err
		}
		if m.isClosed() {
			return VersionedValue{}, errModuleConfigClosed
		}
		return m.store.StateCASContext(ctx, sdk.Scope{Plugin: id}, moduleConfigKey, version, config)
	}()
	if err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	// The configuration is durable now. Retire its old runtime even if the
	// request that committed it has since been canceled.
	reload, cancel := context.WithTimeout(context.Background(), moduleConfigReloadTimeout)
	err = m.Reload(reload)
	cancel()
	notifyChanged()
	snapshot := sdk.ConfigSnapshot{Version: strconv.FormatUint(updated.Version, 10), Values: bytes.Clone(updated.Value)}
	if err != nil {
		return snapshot, fmt.Errorf("module configuration was saved but runtime reload failed: %w", err)
	}
	return snapshot, nil
}

func (view catalogView) configEntry(id string, expected expectedGeneration) (Installed, error) {
	entry, found := view.lookup(id)
	if !found || entry.Manifest.Component != nil {
		return Installed{}, errors.New("module configuration requires an installed root module")
	}
	if entry.Manifest.Contract.Version.Minor < 1 {
		return Installed{}, errors.New("module configuration requires SDK API 1.1 or newer")
	}
	if expected.packageRevision != "" && (expected.packageRevision != entry.Revision || entry.Disabled) {
		return Installed{}, ErrCatalogConflict
	}
	if expected.enableGeneration != nil {
		enablement, err := readObserverEnablement(view.root, id)
		if err != nil {
			return Installed{}, err
		}
		if enablement != *expected.enableGeneration {
			return Installed{}, ErrCatalogConflict
		}
	}
	return entry, nil
}

func moduleConfigValues(values json.RawMessage) error {
	if len(values) == 0 || len(values) > storeStateLimit {
		return errors.New("module configuration must be a bounded JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(values))
	decoder.UseNumber()
	if catalogJSONValue(decoder, 0) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("module configuration must be an unambiguous JSON object")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(values, &object) != nil || object == nil {
		return errors.New("module configuration must be a JSON object")
	}
	return nil
}

func cloneModuleConfig(config sdk.ConfigSnapshot) sdk.ConfigSnapshot {
	return sdk.ConfigSnapshot{Version: config.Version, Values: bytes.Clone(config.Values)}
}
