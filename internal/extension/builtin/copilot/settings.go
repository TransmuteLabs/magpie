package copilot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	SettingsFile = "settings.json"
	CatalogFile  = "providers.json"
	providerName = "magpie"
)

type Model struct {
	ID, Name        string
	Context, Output int
}

type CatalogOptions struct {
	URL, Key string
	Models   func() []Model
}

type ModelOptions struct {
	Model   string
	Catalog *CatalogOptions
}

func PlanModel(options ModelOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	files := settings.Files
	withCatalog := options.Model == "" || options.Catalog != nil
	if withCatalog && (len(files) != 2 || files[1].Path != CatalogFile) ||
		!withCatalog && len(files) != 1 || files[0].Path != SettingsFile {
		return sdk.AgentPlan{}, errors.New("Copilot model plan needs settings.json and any affected providers.json")
	}
	config := files[0]
	var set []edit.KV
	var remove []string
	if options.Model != "" {
		set = []edit.KV{{Path: "model", Value: options.Model}}
	} else if len(bytes.TrimSpace(edit.TrimBOM(config.Body))) != 0 {
		remove = []string{"model"}
	}
	var plan sdk.AgentPlan
	if len(set)+len(remove) != 0 {
		body, err := edit.PatchJSON(config.Body, set, remove)
		if err != nil {
			return sdk.AgentPlan{}, edit.AtFile(config.Path, err)
		}
		appendChange(&plan, config, body)
	}
	if !withCatalog {
		return plan, nil
	}
	catalogPlan, err := planCatalog(files[1], options.Catalog, false)
	if err != nil {
		return sdk.AgentPlan{}, err
	}
	if options.Model == "" {
		plan.Files = append(plan.Files, catalogPlan.Files...)
		return plan, nil
	}
	catalogPlan.Files = append(catalogPlan.Files, plan.Files...)
	return catalogPlan, nil
}

func PlanSync(options CatalogOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != CatalogFile {
		return sdk.AgentPlan{}, errors.New("Copilot catalog sync needs providers.json")
	}
	return planCatalog(settings.Files[0], &options, true)
}

func appendChange(plan *sdk.AgentPlan, file sdk.ActionSettingFile, body []byte) {
	if body != nil && !bytes.Equal(file.Body, body) {
		plan.Files = append(plan.Files, sdk.FileChange{Path: file.Path, Body: body, BeforeHash: file.BeforeHash})
	}
}

func planCatalog(file sdk.ActionSettingFile, options *CatalogOptions, syncOnly bool) (sdk.AgentPlan, error) {
	var plan sdk.AgentPlan
	if options != nil && options.Models == nil {
		return plan, errors.New("Copilot catalog plan needs available models")
	}
	top, err := catalogOf(file.Body)
	if err != nil {
		if syncOnly {
			return plan, nil
		}
		return plan, edit.AtFile(file.Path, err)
	}
	providers, err := catalogList(top, "providers")
	if syncOnly && !slices.ContainsFunc(providers, func(entry json.RawMessage) bool { return owned(entry, "name") }) {
		return plan, nil
	}
	if err != nil {
		return plan, edit.AtFile(file.Path, err)
	}
	models, err := catalogList(top, "models")
	if err != nil {
		return plan, edit.AtFile(file.Path, err)
	}
	if options == nil {
		providerCount, modelCount := len(providers), len(models)
		providers = slices.DeleteFunc(providers, func(entry json.RawMessage) bool { return owned(entry, "name") })
		models = slices.DeleteFunc(models, func(entry json.RawMessage) bool { return owned(entry, "provider") })
		if len(providers) == providerCount && len(models) == modelCount {
			return plan, nil
		}
		if providers == nil {
			providers = []json.RawMessage{}
		}
		if models == nil {
			models = []json.RawMessage{}
		}
	} else {
		providers = placeProvider(providers, *options)
		models = placeModels(models, options.Models())
	}
	top["providers"], _ = json.Marshal(providers)
	top["models"], _ = json.Marshal(models)
	body, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return plan, edit.AtFile(file.Path, err)
	}
	appendChange(&plan, file, append(body, '\n'))
	return plan, nil
}

func catalogOf(raw []byte) (map[string]json.RawMessage, error) {
	top := map[string]json.RawMessage{}
	raw = edit.TrimBOM(raw)
	if len(bytes.TrimSpace(raw)) == 0 {
		return top, nil
	}
	if json.Unmarshal(raw, &top) != nil || top == nil {
		return nil, errors.New("isn't a JSON object, so magpie leaves it as it is")
	}
	return top, nil
}

func catalogList(top map[string]json.RawMessage, key string) ([]json.RawMessage, error) {
	raw, ok := top[key]
	if !ok {
		return nil, nil
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return nil, fmt.Errorf("providers.json's %s isn't a list, so magpie leaves it as it is", key)
	}
	return list, nil
}

func owned(entry json.RawMessage, key string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(entry, &fields) != nil {
		return false
	}
	var name string
	return json.Unmarshal(fields[key], &name) == nil && name == providerName
}

func Provider(raw []byte) map[string]any {
	top, err := catalogOf(raw)
	if err == nil {
		providers, _ := catalogList(top, "providers")
		for _, entry := range providers {
			if owned(entry, "name") {
				var provider map[string]any
				if json.Unmarshal(entry, &provider) == nil {
					return provider
				}
			}
		}
	}
	return nil
}

func placeProvider(providers []json.RawMessage, options CatalogOptions) []json.RawMessage {
	body, _ := json.Marshal(map[string]string{
		"name": providerName, "type": "openai", "baseUrl": options.URL, "apiKey": options.Key,
	})
	var wanted map[string]json.RawMessage
	_ = json.Unmarshal(body, &wanted)
	out := make([]json.RawMessage, 0, len(providers)+1)
	placed := false
	for _, entry := range providers {
		if !owned(entry, "name") {
			out = append(out, entry)
		} else if !placed {
			var current map[string]json.RawMessage
			_ = json.Unmarshal(entry, &current)
			merged, _ := json.Marshal(edit.MergeJSONObjects(current, wanted, nil))
			out = append(out, merged)
			placed = true
		}
	}
	if !placed {
		out = append(out, body)
	}
	return out
}

func placeModels(current []json.RawMessage, models []Model) []json.RawMessage {
	var out []json.RawMessage
	first := -1
	for _, entry := range current {
		if !owned(entry, "provider") {
			out = append(out, entry)
		} else if first < 0 {
			first = len(out)
		}
	}
	if first < 0 {
		first = len(out)
	}
	generated := make([]json.RawMessage, 0, len(models))
	for _, model := range models {
		entry := map[string]any{"id": model.ID, "provider": providerName}
		if model.Name != "" {
			entry["name"] = model.Name
		}
		if model.Output > 0 {
			entry["maxOutputTokens"] = model.Output
		}
		if model.Context > 0 {
			entry["maxContextWindowTokens"] = model.Context
			prompt := model.Context
			if model.Output > 0 && model.Output < prompt {
				prompt -= model.Output
			}
			entry["maxPromptTokens"] = prompt
		}
		body, _ := json.Marshal(entry)
		generated = append(generated, body)
	}
	return slices.Insert(out, first, generated...)
}
