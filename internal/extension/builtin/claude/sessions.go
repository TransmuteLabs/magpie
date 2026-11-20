package claude

import (
	"github.com/yetone/magpie/internal/extension/builtin/sessioncarry"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func ResumeSession() sdk.SessionResumeRecipe {
	return sdk.SessionResumeRecipe{Agent: "claude", Args: []string{"--resume", "{sessionId}"}}
}

func ImportCodexSession() sdk.SessionConverter {
	return sdk.SessionConverter{
		Descriptor: sdk.SessionDescriptor{
			ID: "codex-to-claude", Name: "Import a Codex session",
			From: "codex", To: "claude", OutputDir: ".claude/projects",
		},
		Convert: sessioncarry.FromCodex,
	}
}
