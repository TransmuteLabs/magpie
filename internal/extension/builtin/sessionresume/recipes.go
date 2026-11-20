package sessionresume

import (
	"github.com/yetone/magpie/internal/extension/builtin/claude"
	"github.com/yetone/magpie/internal/extension/builtin/codex"
	"github.com/yetone/magpie/internal/extension/builtin/omp"
	"github.com/yetone/magpie/internal/extension/builtin/pisession"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func Recipes() map[string]sdk.SessionResumeRecipe {
	withID := func(executable, option string) sdk.SessionResumeRecipe {
		return sdk.SessionResumeRecipe{Agent: executable, Args: []string{option, "{sessionId}"}}
	}
	return map[string]sdk.SessionResumeRecipe{
		"claude":    claude.ResumeSession(),
		"codex":     codex.ResumeSession(""),
		"omp":       omp.ResumeSession(""),
		"pi":        pisession.ResumeSession(),
		"opencode":  withID("opencode", "--session"),
		"cline":     withID("cline", "--id"),
		"qoder":     withID("qodercli", "--resume"),
		"qoder-cn":  withID("qoderclicn", "--resume"),
		"grok":      withID("grok", "--resume"),
		"codebuddy": withID("codebuddy", "--resume"),
		"droid":     withID("droid", "--resume"),
		"kimi":      withID("kimi", "--session"),
		"cursor":    withID("cursor-agent", "--resume"),
		"agy":       withID("agy", "--conversation"),
	}
}
