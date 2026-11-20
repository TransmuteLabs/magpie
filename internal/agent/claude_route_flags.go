package agent

import (
	"github.com/yetone/magpie/internal/extension"
	builtinclaude "github.com/yetone/magpie/internal/extension/builtin/claude"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func applyClaudeRouteFlags(path string, at place, options builtinclaude.RouteFlagsOptions) error {
	keys := map[builtinclaude.RouteFlag]string{
		builtinclaude.ReviewFlag:  at.key("claude.auto_mode_server"),
		builtinclaude.SearchFlag:  at.key("claude.tool_search"),
		builtinclaude.ExploreFlag: at.key("claude.explore_inherit_cap"),
	}
	saved := stashShared()
	options.Previous = make(map[builtinclaude.RouteFlag]string, len(keys))
	for flag, key := range keys {
		if value := saved[key]; value != "" {
			options.Previous[flag] = value
		}
	}
	if !options.Routed && !options.Desktop && len(options.Previous) == 0 {
		return nil
	}
	var next map[builtinclaude.RouteFlag]string
	err := extension.ApplyLinkedAgentPlan(options, func(options builtinclaude.RouteFlagsOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
		plan, owned, err := builtinclaude.PlanRouteFlags(options, settings)
		next = owned
		return plan, err
	}, extension.LinkedActionFile{Name: "settings.json", Path: path})
	if err != nil {
		return err
	}
	changed := map[string]string{}
	for flag, key := range keys {
		if next[flag] != options.Previous[flag] {
			changed[key] = next[flag]
		}
	}
	if len(changed) > 0 {
		stash(changed)
	}
	return nil
}
