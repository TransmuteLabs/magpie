package agent

import (
	"strings"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtincopilot "github.com/yetone/magpie/internal/extension/builtin/copilot"
	"github.com/yetone/magpie/internal/gateway"
)

func copilotProvider(path string) map[string]any {
	raw, err := edit.Read(path)
	if err != nil {
		return nil
	}
	return builtincopilot.Provider(raw)
}

func copilotCatalogOptions(url, key string) builtincopilot.CatalogOptions {
	return builtincopilot.CatalogOptions{URL: url, Key: key, Models: func() []builtincopilot.Model {
		models := sharedModels("copilot")
		out := make([]builtincopilot.Model, 0, len(models))
		for _, model := range models {
			out = append(out, builtincopilot.Model{
				ID: model.ID, Name: model.Name, Context: model.Context, Output: maxTokens(model),
			})
		}
		return out
	}}
}

func copilotSetModel(path, catalog, model string) error {
	options := builtincopilot.ModelOptions{Model: model}
	files := []extension.LinkedActionFile{{Name: builtincopilot.SettingsFile, Path: path}}
	if ref, ok := strings.CutPrefix(model, magpieID+"/"); ok && isMagpie(ref) {
		catalog := copilotCatalogOptions(gateway.URL()+"/v1", agentKey("copilot"))
		options.Catalog = &catalog
	}
	if model == "" || options.Catalog != nil {
		files = append(files, extension.LinkedActionFile{Name: builtincopilot.CatalogFile, Path: catalog})
	}
	return extension.ApplyLinkedAgentPlan(options, builtincopilot.PlanModel, files...)
}

func copilotSync(path, url, key string) error {
	return extension.ApplyLinkedAgentPlan(copilotCatalogOptions(url, key), builtincopilot.PlanSync,
		extension.LinkedActionFile{Name: builtincopilot.CatalogFile, Path: path})
}
