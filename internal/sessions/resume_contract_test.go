package sessions

import (
	"runtime"
	"strings"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

var fixedResumeCommands = []struct {
	agent, command string
}{
	{"opencode", "opencode --session"},
	{"cline", "cline --id"},
	{"qoder", "qodercli --resume"},
	{"qoder-cn", "qoderclicn --resume"},
	{"grok", "grok --resume"},
	{"codebuddy", "codebuddy --resume"},
	{"droid", "droid --resume"},
	{"kimi", "kimi --session"},
	{"cursor", "cursor-agent --resume"},
	{"agy", "agy --conversation"},
}

func TestNativeResumeKeepsAgentCommandsAndWorkingDirectory(t *testing.T) {
	const id = "session_2026-11-20"
	const cwd = "/work/agent's project"
	localPrefix := `cd '/work/agent'\''s project' && `
	if runtime.GOOS == "windows" {
		localPrefix = "Set-Location -LiteralPath '/work/agent''s project'; "
	}
	const wslPrefix = `wsl.exe -d 'Ubuntu Test' --cd '/work/agent''s project' -e sh -lc 'exec ${SHELL:-sh} -lic ''`
	for _, test := range fixedResumeCommands {
		t.Run(test.agent, func(t *testing.T) {
			want := test.command + " " + id
			if got := ResumeCommand(test.agent, id, cwd); got != localPrefix+want {
				t.Fatalf("local resume = %q, want %q", got, localPrefix+want)
			}
			if got := resumeFor("Ubuntu Test", test.agent, id, cwd, ""); got != wslPrefix+want+"'''" {
				t.Fatalf("WSL resume = %q, want %q", got, wslPrefix+want+"'''")
			}
			if test.agent == "cursor" {
				want = ""
			}
			if got := ResumeCommand(test.agent, id, ""); got != want {
				t.Fatalf("resume without a directory = %q, want %q", got, want)
			}
		})
	}
}

func TestNativeResumeRejectsInvalidSessionIdentity(t *testing.T) {
	const cwd = "/work/test"
	limit := strings.Repeat("a", 128)
	for _, test := range fixedResumeCommands {
		t.Run(test.agent, func(t *testing.T) {
			if got := ResumeCommand(test.agent, limit, cwd); !strings.HasSuffix(got, test.command+" "+limit) {
				t.Fatalf("resume refused an ID at the SDK limit: %q", got)
			}
			for _, invalid := range []string{"--help", limit + "a", "not;a-command"} {
				if got := ResumeCommand(test.agent, invalid, cwd); got != "" {
					t.Errorf("resume used invalid session identity %q: %q", invalid, got)
				}
			}
		})
	}
}

func TestAgyResumeUsesRecipeWithLocalLaunchOverlay(t *testing.T) {
	previous, registered := linkedSessionResumeRecipes["agy"]
	if !registered {
		t.Fatal("agy has no linked resume recipe")
	}
	oldStart := AgyStart
	t.Cleanup(func() {
		linkedSessionResumeRecipes["agy"] = previous
		AgyStart = oldStart
	})
	linkedSessionResumeRecipes["agy"] = sdk.SessionResumeRecipe{Agent: "agy", Args: []string{"--recipe-fixture", "{sessionId}"}}
	const launch = "GEMINI_API_KEY=k agy --model 'magpie/x'"
	calls := 0
	AgyStart = func() string {
		calls++
		return launch
	}
	if got := ResumeCommand("agy", "sid", ""); got != launch+" --recipe-fixture sid" || calls != 1 {
		t.Fatalf("local overlay = %q, launch calls = %d", got, calls)
	}
	t.Cleanup(ForceCLIGone("agy"))
	const wsl = `wsl.exe -d 'Ubuntu' -e sh -lc 'exec ${SHELL:-sh} -lic ''agy --recipe-fixture sid'''`
	if got := resumeFor("Ubuntu", "agy", "sid", "", ""); got != wsl || calls != 1 {
		t.Fatalf("WSL resume = %q, local launch calls = %d", got, calls)
	}
	if got := resumeFor("", "agy", "sid", "", ""); got != "" || calls != 1 {
		t.Fatalf("resume with agy gone = %q, launch calls = %d", got, calls)
	}
	if got := ResumeCommand("agy", "--help", ""); got != "" || calls != 1 {
		t.Fatalf("rejected identity produced resume = %q, launch calls = %d", got, calls)
	}
}
