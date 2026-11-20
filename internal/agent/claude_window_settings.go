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

const (
	claudePickerKey = "claude.model_picker"
	claudeCapsKey   = "claude.capabilities"
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

func claudeCatalogOptions(at place, managePicker bool) builtinclaude.CatalogOptions {
	saved := stashShared()
	return builtinclaude.CatalogOptions{
		Previous: builtinclaude.CatalogClaims{
			Picker: saved[at.key(claudePickerKey)], Capabilities: saved[at.key(claudeCapsKey)],
		},
		ManagePicker: managePicker,
	}
}

func claudeModelSettingsUpdates(at place, settings builtinclaude.ModelSettingsClaims) map[string]string {
	claims := settings.Window
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
		at.key(claudePickerKey):          settings.Catalog.Picker,
		at.key(claudeCapsKey):            settings.Catalog.Capabilities,
	}
}

func keepClaudeModelSettingsUpdates(at place, claims builtinclaude.ModelSettingsClaims) {
	saved := stashShared()
	updates := claudeModelSettingsUpdates(at, claims)
	for key, value := range updates {
		if saved[key] == value {
			delete(updates, key)
		}
	}
	if len(updates) > 0 {
		stash(updates)
	}
}

func applyClaudeModelSettings(at place, path string, options builtinclaude.ModelSettingsOptions) error {
	var claims builtinclaude.ModelSettingsClaims
	err := extension.ApplyLinkedAgentPlan(options, func(options builtinclaude.ModelSettingsOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
		plan, next, err := builtinclaude.PlanModelSettings(options, settings)
		claims = next
		return plan, err
	}, extension.LinkedActionFile{Name: "settings.json", Path: path})
	if err == nil {
		keepClaudeModelSettingsUpdates(at, claims)
	}
	return err
}
