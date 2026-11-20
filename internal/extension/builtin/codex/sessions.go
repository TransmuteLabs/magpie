package codex

import (
	"github.com/yetone/magpie/internal/extension/builtin/sessioncarry"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func ResumeSession(provider string) sdk.SessionResumeRecipe {
	args := []string{"resume", "{sessionId}"}
	if provider != "" {
		args = []string{"resume", "-c", "model_provider=" + provider, "{sessionId}"}
	}
	return sdk.SessionResumeRecipe{Agent: "codex", Args: args}
}

func ImportClaudeSession() sdk.SessionConverter {
	return sdk.SessionConverter{
		Descriptor: sdk.SessionDescriptor{
			ID: "claude-to-codex", Name: "Import a Claude Code session",
			From: "claude", To: "codex", OutputDir: ".codex/sessions",
		},
		Convert: sessioncarry.FromClaude,
	}
}
