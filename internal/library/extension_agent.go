package library

import (
	"github.com/yetone/magpie/internal/agent"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func extensionAgentTarget(a *agent.Agent, places *sdk.AgentLibraryTarget) *Target {
	target := &Target{
		Agent:        a,
		Instructions: places.Instructions,
		Override:     places.Override,
		Skills:       places.Skills,
		Settings:     places.Settings,
	}
	if places.MCP != "" {
		format, exists := pluginMCP[places.MCPFormat]
		if !exists {
			return nil
		}
		target.MCP = &mcpFile{Path: places.MCP, Format: format}
	}
	return target
}
