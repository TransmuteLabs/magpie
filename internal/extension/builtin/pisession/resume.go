package pisession

import sdk "github.com/yetone/magpie/pkg/extension"

func ResumeSession() sdk.SessionResumeRecipe {
	return sdk.SessionResumeRecipe{
		Agent: "pi",
		Args:  []string{"--session", "{sessionId}"},
	}
}
