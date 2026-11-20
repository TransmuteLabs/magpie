package codex

import (
	"bytes"
	"errors"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type AuxiliaryModelOptions struct {
	MemoryModel *string
	RoutedModel func(string) bool
}

func PlanAuxiliaryModels(options AuxiliaryModelOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != GlobalConfigFile {
		return sdk.AgentPlan{}, errors.New("Codex auxiliary model action requires a config.toml snapshot")
	}
	config := settings.Files[0]
	raw := edit.TrimBOM(config.Body)
	body, err := rewriteAuxiliaryModels(config.Path, raw, options)
	if err != nil {
		return sdk.AgentPlan{}, err
	}
	if bytes.Equal(body, raw) {
		return sdk.AgentPlan{}, nil
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: config.Path, Body: body, BeforeHash: config.BeforeHash}}}, nil
}

func rewriteAuxiliaryModels(path string, raw []byte, options AuxiliaryModelOptions) ([]byte, error) {
	body := raw
	var err error
	for _, pick := range []struct{ table, key string }{
		{"memories", "extract_model"},
		{"memories", "consolidation_model"},
		{"agents", "default_subagent_model"},
	} {
		switch {
		case options.MemoryModel != nil && pick.table == "memories":
			if model := *options.MemoryModel; model != "" {
				body, err = edit.RewriteTOMLKey(path, body, pick.table, pick.key, model)
			} else {
				body, err = edit.RewriteTOMLWithoutKey(path, body, pick.table, pick.key)
			}
		case options.RoutedModel != nil:
			body, err = edit.RewriteTOMLWithoutKeyIf(path, body, pick.table, pick.key, options.RoutedModel)
		}
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}
