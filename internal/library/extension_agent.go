package library

import (
	"path/filepath"

	"github.com/yetone/magpie/internal/agent"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func extensionAgentTarget(a *agent.Agent, places *sdk.AgentLibrary) *Target {
	at := func(name string) string {
		if name == "" {
			return ""
		}
		return filepath.Join(home(), filepath.FromSlash(name))
	}
	target := &Target{Agent: a, Instructions: at(places.Instructions), Skills: at(places.Skills)}
	if places.MCP != "" {
		format, exists := pluginMCP[places.MCPFormat]
		if !exists {
			return nil
		}
		target.MCP = &mcpFile{Path: at(places.MCP), Format: format}
	}
	return target
}
