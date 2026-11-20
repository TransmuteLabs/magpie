package omp

import (
	"path/filepath"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func Library(directory string) sdk.AgentLibraryTarget {
	return sdk.AgentLibraryTarget{
		Instructions: filepath.Join(directory, "AGENTS.md"),
		MCP:          filepath.Join(directory, "mcp.json"),
		MCPFormat:    "omp",
		Skills:       filepath.Join(directory, "skills"),
	}
}
