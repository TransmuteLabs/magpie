package omp

import sdk "github.com/yetone/magpie/pkg/extension"

func ResumeSession(profile string) sdk.SessionResumeRecipe {
	args := []string{"--resume", "{sessionId}"}
	if profile != "" {
		args = append([]string{"--profile", profile}, args...)
	}
	return sdk.SessionResumeRecipe{Agent: "omp", Args: args}
}
