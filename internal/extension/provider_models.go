package extension

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const ModelCatalogPrefix = "models.catalog.v1."

func (m *Manager) PublishModelCatalog(ctx context.Context, entry Installed, configVersion string, scope sdk.Scope, key string, expected uint64, value json.RawMessage) (VersionedValue, error) {
	if !strings.HasPrefix(key, ModelCatalogPrefix) || !storeToken(key) || !entry.Manifest.Contract.Has(sdk.ModelDiscovery) ||
		len(value) > storeStateLimit || !json.Valid(value) {
		return VersionedValue{}, storeInvalid()
	}
	var result VersionedValue
	err := m.withProviderPublication(ctx, entry, configVersion, scope, func(root *os.Root) error {
		var err error
		result, err = compareStoreState(ctx, root, sdk.Scope{Plugin: scope.Plugin}, key, expected, value, true)
		return err
	})
	return result, err
}
