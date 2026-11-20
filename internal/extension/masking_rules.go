package extension

import (
	"context"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type SelectedMaskingRules struct {
	Revision         string
	EnableGeneration string
	Rules            []sdk.MaskingRule
}

// MaskingRules reads declarative data from the frozen installed manifest.
// No worker is acquired and no request data enters this lookup.
func (m *Manager) MaskingRules(ctx context.Context, selection string) (SelectedMaskingRules, bool) {
	module, id, ok := contributionName(selection)
	if !ok {
		return SelectedMaskingRules{}, false
	}
	for _, entry := range m.contributionEntries(ctx) {
		if entry.Manifest.ID != module || !entry.Manifest.Contract.Has(sdk.MaskingRules) {
			continue
		}
		for _, set := range entry.Manifest.MaskingRuleSets {
			if set.ID == id {
				return SelectedMaskingRules{Revision: entry.Revision, EnableGeneration: entry.EnableGeneration, Rules: set.Rules}, true
			}
		}
	}
	return SelectedMaskingRules{}, false
}
