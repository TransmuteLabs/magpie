package agent

import (
	"github.com/yetone/magpie/internal/extension"
	builtinclaude "github.com/yetone/magpie/internal/extension/builtin/claude"
)

func setClaudeEffort(path, model, level string) error {
	options := builtinclaude.EffortPlanOptions{ModelKey: claudeName(model), ReadsTop: claudeReadsTop(model), Level: level}
	return extension.ApplyLinkedAgentPlan(options, builtinclaude.PlanEffort,
		extension.LinkedActionFile{Name: "settings.json", Path: path})
}
