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

func (m *Manager) ModuleConfig(ctx context.Context, id string) (sdk.ConfigSnapshot, error) {
	if err := m.moduleConfigOwner(ctx, id); err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	return m.readModuleConfig(id)
}

func (m *Manager) readModuleConfig(id string) (sdk.ConfigSnapshot, error) {
	value, err := m.store.StateGet(sdk.Scope{Plugin: id}, moduleConfigKey)
	if err != nil {
		return sdk.ConfigSnapshot{}, err
	}
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
	if ctx == nil || m == nil || m.catalog == nil || m.store == nil || !sdk.ValidID(id) {
		return sdk.ConfigSnapshot{}, errors.New("module configuration requires its installed root owner")
	}
	version, err := strconv.ParseUint(expected, 10, 64)
	if err != nil || strconv.FormatUint(version, 10) != expected {
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
		entries, _, err := catalogRead(root)
		if err != nil {
			return VersionedValue{}, err
		}
		for _, entry := range entries {
			if entry.Manifest.ID != id || entry.Manifest.Component != nil {
				continue
			}
			if entry.Manifest.Contract.Version.Minor < 1 {
				return VersionedValue{}, errors.New("module configuration requires SDK API 1.1 or newer")
			}
			if packageRevision != "" && packageRevision != entry.Revision {
				return VersionedValue{}, ErrCatalogConflict
			}
			if err := ctx.Err(); err != nil {
				return VersionedValue{}, err
			}
			return m.store.StateCAS(sdk.Scope{Plugin: id}, moduleConfigKey, version, values)
		}
		return VersionedValue{}, errors.New("module configuration requires an installed root module")
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

func (m *Manager) moduleConfigOwner(ctx context.Context, id string) error {
	if ctx == nil || m == nil || m.store == nil || !sdk.ValidID(id) {
		return errors.New("module configuration requires a manager, context and module identity")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := m.List()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Manifest.ID == id && entry.Manifest.Component == nil {
			if entry.Manifest.Contract.Version.Minor < 1 {
				return errors.New("module configuration requires SDK API 1.1 or newer")
			}
			return nil
		}
	}
	return errors.New("module configuration requires an installed module")
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
