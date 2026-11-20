package opencode

import (
	"path/filepath"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func Library(config string) sdk.AgentLibraryTarget {
	directory := filepath.Dir(config)
	return sdk.AgentLibraryTarget{
		Instructions: filepath.Join(directory, "AGENTS.md"),
		MCP:          config,
		MCPFormat:    "opencode",
		Skills:       filepath.Join(directory, "skills"),
	}
}
