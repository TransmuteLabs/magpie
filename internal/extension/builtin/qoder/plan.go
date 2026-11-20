package qoder

import (
	"bytes"
	"errors"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func Plan(options Options, settings sdk.ActionSettings) (sdk.AgentPlan, Recovery, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != SettingsFile {
		return sdk.AgentPlan{}, Recovery{}, errors.New("Qoder plan needs settings.json")
	}
	file := settings.Files[0]
	body, recovery, err := Patch(options, file.Body)
	if err != nil {
		return sdk.AgentPlan{}, Recovery{}, edit.AtFile(file.Path, err)
	}
	if bytes.Equal(body, file.Body) {
		return sdk.AgentPlan{}, recovery, nil
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: file.Path, Body: body, BeforeHash: file.BeforeHash}}}, recovery, nil
}
