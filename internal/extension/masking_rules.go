package extension

import (
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type SelectedMaskingRules struct {
	Revision         string
	EnableGeneration string
	Rules            []sdk.MaskingRule
}

// MaskingRules reads declarative data from the frozen installed manifest.
// No worker is acquired and no request data enters this lookup.
func (m *Manager) MaskingRules(selection string) (SelectedMaskingRules, bool) {
	module, id, ok := strings.Cut(selection, "/")
	if !ok || !sdk.ValidID(module) || !sdk.ValidID(id) || m.isClosed() {
		return SelectedMaskingRules{}, false
	}
	entries, err := m.List()
	if err != nil {
		return SelectedMaskingRules{}, false
	}
	for _, entry := range entries {
		if entry.Manifest.ID != module || entry.Disabled || entry.observerEnablementErr != nil ||
			!entry.Manifest.Contract.Has(sdk.MaskingRules) {
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
