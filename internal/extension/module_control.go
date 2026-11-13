package extension

import (
	"context"
	"encoding/json"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func (m *Manager) runtimeModuleConfig(manifest sdk.Manifest) (sdk.ConfigSnapshot, error) {
	if manifest.Contract.Version.Minor < 1 {
		return sdk.ConfigSnapshot{Version: "0", Values: json.RawMessage(`{}`)}, nil
	}
	return m.readModuleConfig(manifest.ID)
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
