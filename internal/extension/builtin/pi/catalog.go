package pi

import (
	"errors"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

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
	err = patch(&plan, file, []edit.KV{{Path: key, Value: want}}, nil)
	return plan, err
}
