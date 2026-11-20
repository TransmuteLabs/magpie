package library

import sdk "github.com/yetone/magpie/pkg/extension"

func extensionAgentTarget(target *Target, places *sdk.AgentLibraryTarget) *Target {
	if places.MCP != "" {
		format, exists := pluginMCP[places.MCPFormat]
		if !exists {
			return nil
		}
		target.MCP = &mcpFile{Path: places.MCP, Format: format}
	}
	target.Instructions = places.Instructions
	target.Override = places.Override
	target.Skills = places.Skills
	target.Settings = places.Settings
	return target
}
