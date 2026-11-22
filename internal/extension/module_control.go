package extension

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"sync/atomic"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const moduleConfigMemoLimit = 64 << 10

type moduleConfigMemo struct {
	revision string
	digest   [sha256.Size]byte
	values   json.RawMessage
}

func (m *Manager) runtimeModuleConfig(manifest sdk.Manifest) (sdk.ConfigSnapshot, error) {
	return m.runtimeModuleConfigContext(context.Background(), Installed{Manifest: manifest})
}

func (m *Manager) runtimeModuleConfigContext(ctx context.Context, entry Installed) (sdk.ConfigSnapshot, error) {
	if entry.Manifest.Contract.Version.Minor < 1 {
		return sdk.ConfigSnapshot{Version: "0", Values: json.RawMessage(`{}`)}, nil
	}
	value, err := m.readModuleConfigValue(ctx, entry.Manifest.ID)
	if err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	return m.runtimeModuleConfigValue(entry, value)
}

func (m *Manager) runtimeModuleConfigValue(entry Installed, value VersionedValue) (sdk.ConfigSnapshot, error) {
	manifest := entry.Manifest
	var slot *atomic.Pointer[moduleConfigMemo]
	var digest [sha256.Size]byte
	if entry.Revision != "" && len(value.Value) <= moduleConfigMemoLimit {
		index := int(sha256.Sum256([]byte(manifest.ID))[0]) % len(m.configMemos)
		slot = &m.configMemos[index]
		digest = sha256.Sum256(value.Value)
		if memo := slot.Load(); memo != nil && memo.revision == entry.Revision && memo.digest == digest {
			return sdk.ConfigSnapshot{Version: strconv.FormatUint(value.Version, 10), Values: bytes.Clone(memo.values)}, nil
		}
	}
	config, err := moduleConfigSnapshot(value)
	if err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	config.Values, err = effectiveModuleConfig(manifest, config.Values)
	if err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	if slot != nil && len(config.Values) <= moduleConfigMemoLimit {
		slot.Store(&moduleConfigMemo{revision: entry.Revision, digest: digest, values: bytes.Clone(config.Values)})
	}
	return config, nil
}

func sameModuleConfig(current, expected sdk.ConfigSnapshot) bool {
	if current.Version != expected.Version {
		return false
	}
	if expected.Values == nil || bytes.Equal(current.Values, expected.Values) {
		return true
	}
	parse := func(raw json.RawMessage, values *map[string]any) error {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		return decoder.Decode(values)
	}
	var left, right map[string]any
	return parse(current.Values, &left) == nil && parse(expected.Values, &right) == nil && reflect.DeepEqual(left, right)
}

func effectiveModuleConfig(manifest sdk.Manifest, values json.RawMessage) (json.RawMessage, error) {
	effective, err := manifest.EffectiveModuleConfig(values)
	if err != nil {
		return nil, err
	}
	if len(effective) > storeStateLimit {
		return nil, errors.New("effective module configuration exceeds its size limit")
	}
	return effective, nil
}

func closeModuleClient(client *workerClient) error {
	if client.group == nil {
		client.close()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), componentCloseTimeout)
	defer cancel()
	err := client.group.Close(ctx)
	if err != nil {
		// Revoke the primary byte transport even if native cleanup does not
		// cooperate. Native code remains mapped; its callbacks lose authority.
		client.close()
	}
	return err
}
