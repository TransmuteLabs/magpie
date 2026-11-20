package extension

import (
	"context"
	"slices"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func (m *Manager) ToolModules(ctx context.Context) ([]Installed, error) {
	entries, err := m.readContributionEntries(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(entries, func(entry Installed) bool {
		return !entry.Manifest.Contract.Has(sdk.ToolProvider) || len(entry.Manifest.Tools) == 0
	}), nil
}
