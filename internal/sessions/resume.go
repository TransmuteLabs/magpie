package sessions

import (
	"context"
	"strings"
	"time"

	builtinclaude "github.com/yetone/magpie/internal/extension/builtin/claude"
	builtincodex "github.com/yetone/magpie/internal/extension/builtin/codex"
	builtinomp "github.com/yetone/magpie/internal/extension/builtin/omp"
	"github.com/yetone/magpie/internal/lazyre"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var linkedSessionResumeRecipes = map[string]sdk.SessionResumeRecipe{
	"claude": builtinclaude.ResumeSession(),
	"codex":  builtincodex.ResumeSession(""),
}

var plainResumeArg = lazyre.New(`^[0-9A-Za-z_.=-]+$`)

func linkedResumeCommand(recipe sdk.SessionResumeRecipe, id string) string {
	argv, err := recipe.Arguments(id)
	if err != nil || !sdk.ValidID(recipe.Agent) {
		return ""
	}
	for _, argument := range argv {
		if !plainResumeArg.MatchString(argument) {
			return ""
		}
	}
	return recipe.Agent + " " + strings.Join(argv, " ")
}

func ompResumeCommand(path, id string) string {
	profile := ompProfile(path)
	if profile != "" && !safeOmpProfile.MatchString(profile) {
		return ""
	}
	return linkedResumeCommand(builtinomp.ResumeSession(profile), id)
}

func codexResumeOnProvider(saved, id, provider string) string {
	plain := linkedResumeCommand(linkedSessionResumeRecipes["codex"], id)
	selected := linkedResumeCommand(builtincodex.ResumeSession(provider), id)
	if plain != "" && selected != "" && strings.HasSuffix(saved, plain) {
		return strings.TrimSuffix(saved, plain) + selected
	}
	return saved
}

func listedResume(session Session) string {
	if session.source != nil {
		return sourceResume(session.source)
	}
	if session.Resume != "" || session.ReadOnly {
		return session.Resume
	}
	return resumeFor(session.WSL, session.Agent, session.ID, session.Cwd, session.Path)
}

// ResumeNow prepares the command when the user opens a listed session.
func ResumeNow(session Session) string {
	return ResumeNowContext(context.Background(), session)
}

// ResumeNowContext returns the launch command while the session's selected
// owner still has authority. A delegated command retains its source identity.
func ResumeNowContext(ctx context.Context, session Session) string {
	if session.source != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	owner, err := selectSessionHistoryOwner(ctx, session)
	if err != nil {
		return ""
	}
	admit := func() error { return owner.check(ctx) }
	run := session.Resume
	switch {
	case owner.mode == historyDelegated:
		run = sourceResume(session.source)
	case session.Agent == "codex":
		run = resumeCodex(ctx, session, admit)
	}
	if admit() != nil {
		return ""
	}
	return run
}
