package codex

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type AuxiliaryModelOptions struct {
	MemoryModel *string
	RoutedModel func(string) bool
	Subagent    *SubagentOptions
}

type SubagentOptions struct {
	Model       *string
	Effort      *string
	DropModel   bool
	IsMagpie    func(string) bool
	GatewayBase func(string) bool
	Efforts     func(model string, asProvider, viaBase bool) []string
}

const (
	subagentTable  = "agents"
	subagentModel  = "default_subagent_model"
	subagentEffort = "default_subagent_reasoning_effort"
)

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
		{subagentTable, subagentModel},
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
	if options.Subagent != nil {
		return rewriteSubagent(path, body, *options.Subagent)
	}
	return body, nil
}

func rewriteSubagent(path string, raw []byte, options SubagentOptions) ([]byte, error) {
	if options.Model != nil && *options.Model == "" {
		return edit.RewriteTOMLWithoutKey(path, raw, subagentTable, subagentModel)
	}
	if options.Effort != nil && *options.Effort == "" {
		return edit.RewriteTOMLWithoutKey(path, raw, subagentTable, subagentEffort)
	}
	top, agents, err := edit.ReadTOMLTableSnapshot(path, raw, subagentTable)
	if err != nil {
		return nil, err
	}
	model := agents.Scalar(subagentModel)
	effort := agents.Scalar(subagentEffort)
	asProvider := top("model_provider") == "magpie"
	viaBase := func() bool { return !asProvider && options.GatewayBase(top("openai_base_url")) }
	switch {
	case options.Model != nil:
		model = *options.Model
		if options.IsMagpie(model) && !asProvider && !viaBase() {
			return nil, errors.New("pick a model through magpie for Codex first; its subagents can then have one of their own")
		}
		raw, err = edit.RewriteTOMLKey(path, raw, subagentTable, subagentModel, model)
	case options.DropModel:
		if model == "" {
			return raw, nil
		}
		raw, err = edit.RewriteTOMLWithoutKey(path, raw, subagentTable, subagentModel)
		model = ""
	}
	if err != nil {
		return nil, err
	}
	if options.Effort != nil {
		effort = *options.Effort
	}
	if effort == "" {
		return raw, nil
	}
	model = cmp.Or(model, top("model"))
	levels := options.Efforts(model, asProvider, viaBase())
	if len(levels) > 0 && !slices.Contains(levels, effort) {
		if options.Effort != nil {
			return nil, fmt.Errorf("Codex runs %s at %s, not %q: it would refuse to start a subagent", model, strings.Join(levels, ", "), effort)
		}
		return edit.RewriteTOMLWithoutKey(path, raw, subagentTable, subagentEffort)
	}
	if options.Effort != nil {
		return edit.RewriteTOMLKey(path, raw, subagentTable, subagentEffort, effort)
	}
	return raw, nil
}
