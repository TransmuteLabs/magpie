package codex

import (
	"path/filepath"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func Library(directory string) sdk.AgentLibraryTarget {
	return sdk.AgentLibraryTarget{
		Instructions: filepath.Join(directory, "AGENTS.md"),
		Override:     filepath.Join(directory, "AGENTS.override.md"),
		MCP:          filepath.Join(directory, "config.toml"),
		MCPFormat:    "codex",
		Skills:       filepath.Join(directory, "skills"),
	}
}
