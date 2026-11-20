package extension

import (
	"context"
	"encoding/json"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func (m *Manager) runtimeModuleConfig(manifest sdk.Manifest) (sdk.ConfigSnapshot, error) {
	return m.runtimeModuleConfigContext(context.Background(), manifest)
}

func (m *Manager) runtimeModuleConfigContext(ctx context.Context, manifest sdk.Manifest) (sdk.ConfigSnapshot, error) {
	if manifest.Contract.Version.Minor < 1 {
		return sdk.ConfigSnapshot{Version: "0", Values: json.RawMessage(`{}`)}, nil
	}
	config, err := m.readModuleConfigContext(ctx, manifest.ID)
	if err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	if err := manifest.ValidateProviderOptionValues(config.Values); err != nil {
		return sdk.ConfigSnapshot{}, err
	}
	return config, nil
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
