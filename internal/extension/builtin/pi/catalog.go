package pi

import (
	"errors"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type CatalogSelection struct {
	Model   string
	Refresh func(current string) any
}

func PlanCatalogSelection(options CatalogSelection, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if options.Model == "" || options.Refresh == nil || len(settings.Files) != 2 ||
		settings.Files[0].Path != SettingsFile || settings.Files[1].Path != CatalogFile {
		return sdk.AgentPlan{}, errors.New("Pi catalog selection requires a model, settings and catalog")
	}
	model, err := PlanModel(options.Model, sdk.ActionSettings{Files: settings.Files[:1]})
	if err != nil {
		return sdk.AgentPlan{}, err
	}
	const key = "providers.magpie"
	file := settings.Files[1]
	current := parseSettings(file.Body).Get(key)
	value := options.Refresh(current.Raw)
	var plan sdk.AgentPlan
	if !edit.SameJSON(current.Raw, value) {
		if err := patch(&plan, file, []edit.KV{{Path: key, Value: value}}, nil); err != nil {
			return sdk.AgentPlan{}, err
		}
	}
	plan.Files = append(plan.Files, model.Files...)
	return plan, nil
}

func PlanCatalogPick(build func(current string) (any, error), settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	var plan sdk.AgentPlan
	if len(settings.Files) != 1 || settings.Files[0].Path != CatalogFile {
		return plan, errors.New("Pi protocol action requires a models.json snapshot")
	}
	const key = "providers.magpie"
	file := settings.Files[0]
	current := parseSettings(file.Body).Get(key)
	if !current.Exists() {
		return plan, nil
	}
	want, err := build(current.Raw)
	if err != nil {
		return plan, err
	}
	if edit.SameJSON(current.Raw, want) {
		return plan, nil
	}
	err = patch(&plan, file, []edit.KV{{Path: key, Value: want}}, nil)
	return plan, err
}

type CatalogSync struct {
	Refresh func(current string) (any, error)
	Scope   func(ScopeSettings) ([]string, bool)
}

func PlanCatalogSync(options CatalogSync, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 2 || settings.Files[0].Path != SettingsFile || settings.Files[1].Path != CatalogFile {
		return sdk.AgentPlan{}, errors.New("Pi catalog Sync requires settings and catalog snapshots")
	}
	var plan sdk.AgentPlan
	if options.Refresh != nil {
		var err error
		plan, err = PlanCatalogPick(options.Refresh, sdk.ActionSettings{Files: settings.Files[1:]})
		if err != nil {
			return sdk.AgentPlan{}, err
		}
	}
	scopeInput := [2]sdk.ActionSettingFile{settings.Files[0], settings.Files[1]}
	if len(plan.Files) > 0 {
		scopeInput[1].Body = plan.Files[0].Body
	}
	scope, err := PlanScope(options.Scope, sdk.ActionSettings{Files: scopeInput[:]})
	if err != nil {
		return sdk.AgentPlan{}, err
	}
	plan.Files = append(plan.Files, scope.Files...)
	return plan, nil
}
