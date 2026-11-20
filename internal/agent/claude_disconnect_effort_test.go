package agent

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func TestClaudeDisconnectRejectsConcurrentEffortEdit(t *testing.T) {
	home := syncHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, path, `{"model":"sonnet","effortLevel":"xhigh","env":{"USER_SETTING":"keep"}}`+"\n")
	agent := claude(home)
	if err := agent.Apply("model", "relay/glm-4.6"); err != nil {
		t.Fatal(err)
	}
	if err := agent.Apply("effort", "max"); err != nil {
		t.Fatal(err)
	}
	if !agent.Wired() || appliedOf(agent.ID).Fields["effort"] != "max" || stashLoad()["claude.model"] != "sonnet" {
		t.Fatal("fixture did not record Claude's model and Magpie's effort")
	}
	target, err := edit.Target(path)
	if err != nil {
		t.Fatal(err)
	}
	var userSettings string
	err = edit.Filtered(func(path string, proposed []byte) []byte {
		if path == target && userSettings == "" && !strings.Contains(string(proposed), claudeEffortEnv) {
			before := readFile(path)
			own := `"` + claudeEffortEnv + `": "max"`
			userSettings = strings.Replace(before, own, `"`+claudeEffortEnv+`": "high"`, 1)
			if userSettings == before {
				t.Fatal("effort removal did not start from an owned override")
			}
			writeFile(t, path, userSettings)
		}
		return proposed
	}, agent.Disconnect)
	if userSettings == "" || !errors.Is(err, extension.ErrActionChanged) {
		t.Errorf("expected concurrent edit conflict: injected=%v, err=%v", userSettings != "", err)
	}
	if got := readFile(path); got != userSettings {
		t.Errorf("disconnect overwrote user's settings: got %q, want %q", got, userSettings)
	}
	if got := stashLoad()["claude.model"]; got != "sonnet" {
		t.Errorf("model stash = %q, want sonnet", got)
	}
}
