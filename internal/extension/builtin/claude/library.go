package claude

import (
	"path/filepath"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func Library(home, directory string, custom, profile bool) sdk.AgentLibraryTarget {
	mcpDirectory := home
	if custom || profile && filepath.Clean(directory) != filepath.Join(home, ".claude") {
		mcpDirectory = directory
	}
	target := sdk.AgentLibraryTarget{
		Instructions: filepath.Join(directory, "CLAUDE.md"),
		MCP:          filepath.Join(mcpDirectory, ".claude.json"),
		MCPFormat:    "claude",
		Skills:       filepath.Join(directory, "skills"),
	}
	if !profile {
		target.Settings = filepath.Join(directory, "settings.json")
	}
	return target
}
