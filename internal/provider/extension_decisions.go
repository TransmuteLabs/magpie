package provider

import (
	"slices"

	"github.com/yetone/magpie/internal/catalog"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func (binding *extensionAccount) decisionModel(model string) bool {
	if binding.manifest.Contract.Version.Minor < 7 || !binding.manifest.Contract.Has(sdk.ModelDiscovery) ||
		!binding.descriptor.SupportsOperation(sdk.OperationSystemOne) {
		return false
	}
	models, err := binding.models()
	return err == nil && slices.ContainsFunc(models, func(entry catalog.Model) bool {
		return entry.ID == model && entry.Decides && len(entry.APIs) == 0
	})
}

func (p Provider) extensionDecisionModels() []catalog.Model {
	models := p.Account.models()
	return slices.DeleteFunc(slices.Clone(models), func(model catalog.Model) bool { return !model.Decides })
}

func (p Provider) ExtensionListsDecisionModel(model string) bool {
	return p.IsExtension() && slices.ContainsFunc(p.extensionDecisionModels(), func(entry catalog.Model) bool {
		return entry.ID == model
	})
}
