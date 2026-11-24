package sessions

import (
	"cmp"
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	builtincodex "github.com/yetone/magpie/internal/extension/builtin/codex"
	builtinomp "github.com/yetone/magpie/internal/extension/builtin/omp"
	"github.com/yetone/magpie/internal/extension/builtin/sessionresume"
	"github.com/yetone/magpie/internal/lazyre"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var linkedSessionResumeRecipes = sessionresume.Recipes()

var plainResumeArg = lazyre.New(`^[0-9A-Za-z_.=-]+$`)
var safeOmpProfile = lazyre.New(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func linkedResumeCommand(recipe sdk.SessionResumeRecipe, id string, launch func() string) string {
	if !safeID.MatchString(id) {
		return ""
	}
	argv, err := recipe.Arguments(id)
	if err != nil || !sdk.ValidID(recipe.Agent) {
		return ""
	}
	for _, argument := range argv {
		if !plainResumeArg.MatchString(argument) {
			return ""
		}
	}
	start := recipe.Agent
	if launch != nil {
		start = cmp.Or(launch(), start)
	}
	return start + " " + strings.Join(argv, " ")
}

// ResumeCommand is the shell line that picks a session up again in its
// folder, or "" for an id that isn't plain.
func ResumeCommand(agent, id, cwd string) string { return resumeCommand("", agent, id, cwd, "") }

// resumeFor is resumeCommand for a listed row: none for an agent whose
// command is gone from this computer, which the line would fail to start.
// A WSL distro's agent is looked for there, not here.
func resumeFor(distro, agent, id, cwd, path string) string {
	if distro == "" && cliGone(agent) {
		return ""
	}
	return resumeCommand(distro, agent, id, cwd, path)
}

func resumeCommand(distro, agent, id, cwd, path string) string {
	recipe, found := linkedSessionResumeRecipes[agent]
	if !found || agent == "cursor" && cwd == "" {
		return ""
	}
	if agent == "omp" {
		if profile := ompProfile(path); profile != "" {
			return resumeLine(distro, cwd, namedOMPResumeCommand(id, profile))
		}
		if distro == "" && path != "" {
			if active, err := extensionOMPProfile(); err == nil && active != "" {
				recipe = builtinomp.ResumeSession("default")
			} else if filepath.IsAbs(path) {
				run := linkedResumeCommand(recipe, id, nil)
				return resumeLine("", cwd, defaultOMPResumeRun(run))
			}
		}
	}
	var launch func() string
	if agent == "agy" && distro == "" {
		launch = AgyStart
	}
	return resumeLine(distro, cwd, linkedResumeCommand(recipe, id, launch))
}

func defaultOMPResumeRun(run string) string {
	if run == "" {
		return ""
	}
	if runtime.GOOS == "windows" {
		return "& { $savedOMPProfile = $env:OMP_PROFILE; try { $env:OMP_PROFILE = 'default'; " +
			run + " } finally { $env:OMP_PROFILE = $savedOMPProfile } }"
	}
	return "OMP_PROFILE=default " + run
}

func namedOMPResumeCommand(id, profile string) string {
	if !safeOmpProfile.MatchString(profile) {
		return ""
	}
	return linkedResumeCommand(builtinomp.ResumeSession(profile), id, nil)
}

func resumeLine(distro, cwd, run string) string {
	if run == "" {
		return ""
	}
	if distro != "" {
		return wslResume(distro, cwd, run)
	}
	if cwd == "" {
		return run
	}
	if runtime.GOOS == "windows" {
		return "Set-Location -LiteralPath '" + strings.ReplaceAll(cwd, "'", "''") + "'; " + run
	}
	return "cd " + shellQuote(cwd) + " && " + run
}

func codexResumeOnProvider(saved, id, provider string) string {
	plain := linkedResumeCommand(linkedSessionResumeRecipes["codex"], id, nil)
	selected := linkedResumeCommand(builtincodex.ResumeSession(provider), id, nil)
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
