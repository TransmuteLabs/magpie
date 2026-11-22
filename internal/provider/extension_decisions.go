package provider

import (
	"context"
	"slices"

	"github.com/yetone/magpie/internal/catalog"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func (binding *extensionAccount) decisionModel(model string) bool {
	return binding.decisionModelContext(context.Background(), model)
}

func (binding *extensionAccount) decisionModelContext(ctx context.Context, model string) bool {
	if binding.manifest.Contract.Version.Minor < 7 || !binding.manifest.Contract.Has(sdk.ModelDiscovery) ||
		!binding.descriptor.SupportsOperation(sdk.OperationSystemOne) {
		return false
	}
	models, err := binding.modelsContext(ctx)
	return err == nil && slices.ContainsFunc(models, func(entry catalog.Model) bool {
		return entry.ID == model && entry.Decides && len(entry.APIs) == 0
	})
}

func (p Provider) extensionDecisionModels() []catalog.Model {
	return p.extensionDecisionModelsContext(context.Background())
}

func (p Provider) extensionDecisionModelsContext(ctx context.Context) []catalog.Model {
	models := p.Account.listedModels(ctx)
	return slices.DeleteFunc(slices.Clone(models), func(model catalog.Model) bool { return !model.Decides })
}

func (p Provider) ExtensionListsDecisionModel(model string) bool {
	return p.IsExtension() && slices.ContainsFunc(p.extensionDecisionModels(), func(entry catalog.Model) bool {
		return entry.ID == model
	})
}
