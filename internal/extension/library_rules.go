package extension

import (
	"context"
	"fmt"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type LibraryRule struct {
	Name     string `json:"name"`
	Revision string `json:"revision"`
	sdk.LibraryRuleDescriptor
}

func (m *Manager) LibraryRules(ctx context.Context) ([]LibraryRule, error) {
	entries, err := m.ActiveModules(ctx)
	if err != nil {
		return nil, err
	}
	rules := []LibraryRule{}
	for _, entry := range entries {
		for _, contribution := range entry.Manifest.Contributions {
			if contribution.Point == sdk.ContributionLibraryRule && contribution.Version == 1 {
				rules = append(rules, m.libraryRule(entry, *contribution.LibraryRule))
			}
		}
	}
	return rules, nil
}

func (m *Manager) LibraryRule(ctx context.Context, name, expected string) (LibraryRule, error) {
	module, id, valid := contributionName(name)
	if !valid || !catalogDigest(expected) {
		return LibraryRule{}, fmt.Errorf("select a Library rule by module/id and its listed revision")
	}
	entry, err := m.ActiveModule(ctx, module)
	if err != nil {
		return LibraryRule{}, err
	}
	for _, contribution := range entry.Manifest.Contributions {
		if contribution.Point == sdk.ContributionLibraryRule && contribution.Version == 1 && contribution.LibraryRule.ID == id {
			rule := m.libraryRule(entry, *contribution.LibraryRule)
			if rule.Revision == expected {
				return rule, nil
			}
			break
		}
	}
	return LibraryRule{}, errContributionRevoked
}

func (m *Manager) libraryRule(entry Installed, descriptor sdk.LibraryRuleDescriptor) LibraryRule {
	name := entry.Manifest.ID + "/" + descriptor.ID
	return LibraryRule{
		Name: name, Revision: storeHash(m.Namespace(), name, entry.Revision, entry.EnableGeneration),
		LibraryRuleDescriptor: descriptor,
	}
}
