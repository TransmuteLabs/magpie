package sessions

import (
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
)

func TestContentOfChecksNativeHistoryOwner(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []string{"before", "during"} {
			t.Run(agent+"/"+phase, func(t *testing.T) {
				setup(t)
				path := filepath.Join(t.TempDir(), "history.jsonl")
				line := claudeUser(1, `"call preview content"`)
				if agent == "codex" {
					line = cxSaid("user", "call preview content")
				}
				writeLines(t, path, line)
				call := Call{Agent: agent, File: path, To: math.MaxInt64}
				native, err := ContentOf(call)
				if err != nil || len(native.Input) != 1 || native.Input[0].Text != "call preview content" {
					t.Fatalf("native preview = %+v, %v", native, err)
				}
				if phase == "before" {
					mutationHistoryBinding(t, agent)
				}
				scans := 0
				previous := scanHook
				scanHook = func(name string, _ int64) {
					if name == path {
						scans++
						if phase == "during" {
							mutationHistoryBinding(t, agent)
						}
					}
				}
				t.Cleanup(func() { scanHook = previous })
				preview, err := ContentOf(call)
				if !errors.Is(err, extension.ErrActionChanged) || len(preview.Input) != 0 || len(preview.Output) != 0 {
					t.Errorf("call preview escaped the native owner check: %+v, %v", preview, err)
				}
				if phase == "before" && scans != 0 || phase == "during" && scans != 1 {
					t.Errorf("unexpected scans at %s delegation: %d", phase, scans)
				}
			})
		}
	}
}

func TestDesktopCallKeepsItsSourceOwner(t *testing.T) {
	for _, agent := range []string{"claude", "claude-desktop"} {
		t.Run(agent, func(t *testing.T) {
			d := setupCalls(t)
			t.Setenv("XDG_CONFIG_HOME", "")
			t.Setenv("APPDATA", filepath.Join(t.TempDir(), "appdata"))
			file := filepath.Join(d.claude, "projects", "-work-app", "sess1.jsonl")
			line := strings.Replace(claudeBlocks("desktop", 2, `{"type":"text","text":"Desktop answer"}`),
				`"sessionId":`, `"entrypoint":"claude-desktop-3p","sessionId":`, 1)
			writeLines(t, file, claudeUser(1, `"Desktop question"`), line)
			end := callT0.Add(2 * time.Second)
			call, ok := FindCall("claude-desktop", "sess1", callT0, end, end)
			if !ok || call.Agent != "claude-desktop" {
				t.Fatalf("Desktop Code tab call = %+v", call)
			}
			content, err := call.Content()
			if err != nil || len(content.Output) != 1 || content.Output[0].Text != "Desktop answer" {
				t.Fatalf("Desktop Code tab preview = %+v, %v", content, err)
			}
			mutationHistoryBinding(t, agent)
			content, err = call.Content()
			if !errors.Is(err, extension.ErrActionChanged) || len(content.Input) != 0 || len(content.Output) != 0 {
				t.Errorf("preview after delegating %s = %+v, %v", agent, content, err)
			}
			if _, ok := FindCall("claude-desktop", "sess1", callT0, end, end); ok {
				t.Errorf("Desktop Code tab call still listed after delegating %s", agent)
			}
		})
	}
}
