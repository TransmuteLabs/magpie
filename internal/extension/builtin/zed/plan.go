package zed

import (
	"bytes"
	"errors"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func Plan(options Options, settings sdk.ActionSettings) (sdk.AgentPlan, Effects, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != SettingsFile {
		return sdk.AgentPlan{}, Effects{}, errors.New("Zed plan needs settings.json")
	}
	file := settings.Files[0]
	body, effects, err := Patch(options, file.Body)
	if err != nil {
		return sdk.AgentPlan{}, Effects{}, edit.AtFile(file.Path, err)
	}
	if bytes.Equal(file.Body, body) {
		return sdk.AgentPlan{}, effects, nil
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: file.Path, BeforeHash: file.BeforeHash, Body: body}}}, effects, nil
}
