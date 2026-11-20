package agent

import (
	"encoding/json"
	"strings"

	"github.com/yetone/magpie/internal/extension"
	builtinclaude "github.com/yetone/magpie/internal/extension/builtin/claude"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type claudeWindowMode uint8

const (
	claudeRouteWindows claudeWindowMode = iota
	claudeDesktopWindows
	claudeRestoreWindows
)

func claudeWindowOptions(at place, main string, mode claudeWindowMode) builtinclaude.WindowOptions {
	saved := stashShared()
	previous := builtinclaude.WindowClaims{
		Context: saved[at.key("claude.context_tokens")],
		Default: saved[at.key("claude.compact_default")],
		Compact: saved[at.key("claude.compact_window")],
		Long:    saved[at.key("claude.compact_long")] == "1",
	}
	_ = json.Unmarshal([]byte(saved[at.key("claude.model_budgets")]), &previous.Budgets)
	model := provider.ClaudeDashed(strings.TrimSuffix(strings.TrimPrefix(main, "magpie/"), "[1m]"))
	return builtinclaude.WindowOptions{
		Previous: previous, ManageCompact: mode != claudeDesktopWindows,
		MatchesModel: func(id string) bool {
			return strings.EqualFold(provider.ClaudeDashed(strings.TrimSuffix(id, "[1m]")), model)
		},
	}
}

func claudeWindowUpdates(at place, claims builtinclaude.WindowClaims) map[string]string {
	var budgets, long string
	if len(claims.Budgets) > 0 {
		encoded, _ := json.Marshal(claims.Budgets)
		budgets = string(encoded)
	}
	if claims.Long {
		long = "1"
	}
	return map[string]string{
		at.key("claude.context_tokens"):  claims.Context,
		at.key("claude.compact_default"): claims.Default,
		at.key("claude.compact_window"):  claims.Compact,
		at.key("claude.compact_long"):    long,
		at.key("claude.model_budgets"):   budgets,
	}
}

func keepClaudeWindowUpdates(at place, claims builtinclaude.WindowClaims) {
	saved := stashShared()
	updates := claudeWindowUpdates(at, claims)
	for key, value := range updates {
		if saved[key] == value {
			delete(updates, key)
		}
	}
	if len(updates) > 0 {
		stash(updates)
	}
}

func applyClaudeWindowSettings(at place, path string, options builtinclaude.WindowOptions) error {
	var claims builtinclaude.WindowClaims
	err := extension.ApplyLinkedAgentPlan(options, func(options builtinclaude.WindowOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
		plan, next, err := builtinclaude.PlanWindowSettings(options, settings)
		claims = next
		return plan, err
	}, extension.LinkedActionFile{Name: "settings.json", Path: path})
	if err == nil {
		keepClaudeWindowUpdates(at, claims)
	}
	return err
}
