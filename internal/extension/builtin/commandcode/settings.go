package commandcode

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	SettingsFile = "settings.json"
	CatalogFile  = "providers.json"
	catalogKey   = "provider.magpie"
	effortKey    = "reasoningEffort"
)

type CatalogOptions struct {
	Value func(current string) (value any, changed bool)
}

type ModelOptions struct {
	Model   string
	Catalog CatalogOptions
}

func PlanModel(options ModelOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	files := settings.Files
	if len(files) != 2 || files[0].Path != SettingsFile || files[1].Path != CatalogFile {
		return sdk.AgentPlan{}, errors.New("Command Code model plan needs settings.json and providers.json")
	}
	config, catalog := files[0], files[1]
	var set []edit.KV
	var remove []string
	if options.Model == "" {
		remove = append(remove, "model")
	} else {
		set = append(set, edit.KV{Path: "model", Value: options.Model})
	}
	if options.Catalog.Value != nil {
		set = append(set, edit.KV{Path: "modelProvider", Value: "magpie"})
	} else if gjson.GetBytes(jsonc.ToJSON(edit.TrimBOM(config.Body)), "modelProvider").String() == "magpie" {
		remove = append(remove, "modelProvider")
	}
	plan, err := patch(sdk.AgentPlan{}, config, set, remove)
	if err != nil {
		return sdk.AgentPlan{}, err
	}
	if options.Catalog.Value == nil {
		return patch(plan, catalog, nil, []string{catalogKey})
	}
	catalogPlan, err := planCatalog(options.Catalog, catalog, false)
	if err != nil {
		return sdk.AgentPlan{}, err
	}
	catalogPlan.Files = append(catalogPlan.Files, plan.Files...)
	return catalogPlan, nil
}

func PlanSync(options CatalogOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != CatalogFile || options.Value == nil {
		return sdk.AgentPlan{}, errors.New("Command Code catalog sync needs providers.json and a catalog")
	}
	return planCatalog(options, settings.Files[0], true)
}

func planCatalog(options CatalogOptions, file sdk.ActionSettingFile, onlyExisting bool) (sdk.AgentPlan, error) {
	current := gjson.GetBytes(jsonc.ToJSON(edit.TrimBOM(file.Body)), catalogKey)
	if onlyExisting && !current.Exists() {
		return sdk.AgentPlan{}, nil
	}
	value, changed := options.Value(current.String())
	if !changed {
		return sdk.AgentPlan{}, nil
	}
	return patch(sdk.AgentPlan{}, file, []edit.KV{{Path: catalogKey, Value: value}}, nil)
}

func PlanEffort(effort string, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != SettingsFile {
		return sdk.AgentPlan{}, errors.New("Command Code effort plan needs settings.json")
	}
	file := settings.Files[0]
	raw := jsonc.ToJSON(edit.TrimBOM(file.Body))
	model := gjson.GetBytes(raw, "model").String()
	if model == "" {
		return sdk.AgentPlan{}, errors.New("pick Command Code's model first; it keeps an effort for each model")
	}
	efforts := map[string]json.RawMessage{}
	if existing := gjson.GetBytes(raw, effortKey); existing.Exists() {
		if err := json.Unmarshal([]byte(existing.Raw), &efforts); err != nil {
			return sdk.AgentPlan{}, edit.AtFile(file.Path, err)
		}
	}
	if effort == "" {
		delete(efforts, model)
		if len(efforts) == 0 {
			return patch(sdk.AgentPlan{}, file, nil, []string{effortKey})
		}
	} else {
		if efforts == nil {
			efforts = map[string]json.RawMessage{}
		}
		value, err := json.Marshal(effort)
		if err != nil {
			return sdk.AgentPlan{}, err
		}
		efforts[model] = value
	}
	return patch(sdk.AgentPlan{}, file, []edit.KV{{Path: effortKey, Value: efforts}}, nil)
}

func patch(plan sdk.AgentPlan, file sdk.ActionSettingFile, set []edit.KV, remove []string) (sdk.AgentPlan, error) {
	if len(set) == 0 && len(bytes.TrimSpace(edit.TrimBOM(file.Body))) == 0 {
		return plan, nil
	}
	body, err := edit.PatchJSON(file.Body, set, remove)
	if err != nil {
		return sdk.AgentPlan{}, edit.AtFile(file.Path, err)
	}
	if !bytes.Equal(body, file.Body) {
		plan.Files = append(plan.Files, sdk.FileChange{Path: file.Path, BeforeHash: file.BeforeHash, Body: body})
	}
	return plan, nil
}
