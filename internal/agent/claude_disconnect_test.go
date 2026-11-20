package agent

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func connectedClaudeForDisconnect(t *testing.T) *Agent {
	t.Helper()
	home := syncHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	settings := "\xef\xbb\xbf" + strings.ReplaceAll(`{
  // User preferences
  "model": "sonnet",
  "effortLevel": "xhigh",
  "modelSettings": {"claude-opus-5-5": {"effortLevel": "low", "maxEffortLevel": "high"}},
  "env": {"USER_SETTING": "keep"}
}
`, "\n", "\r\n")
	writeFile(t, path, settings)
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
	return agent
}

func TestClaudeDisconnectPreservesEffortOwnership(t *testing.T) {
	effort := "env." + claudeEffortEnv
	for _, scenario := range []struct {
		name, userLevel string
	}{
		{name: "owned"},
		{name: "hand edited", userLevel: "medium"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			agent := connectedClaudeForDisconnect(t)
			if scenario.userLevel != "" {
				if err := edit.SetJSON(agent.Path, edit.KV{Path: effort, Value: scenario.userLevel}); err != nil {
					t.Fatal(err)
				}
			}
			if err := agent.Disconnect(); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{
				effort:             scenario.userLevel,
				"model":            "sonnet",
				"effortLevel":      "xhigh",
				"env.USER_SETTING": "keep",
				"modelSettings.claude-opus-5-5.effortLevel":    "low",
				"modelSettings.claude-opus-5-5.maxEffortLevel": "high",
			} {
				if got, exists := edit.GetJSON(agent.Path, key); got != want || exists != (want != "") {
					t.Errorf("%s = %q, present=%v; want %q", key, got, exists, want)
				}
			}
			got := readFile(agent.Path)
			if !strings.HasPrefix(got, "\xef\xbb\xbf") || strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") ||
				!strings.Contains(got, "  // User preferences\r\n") {
				t.Errorf("settings formatting changed: %q", got)
			}
			if agent.Wired() || stashLoad()["claude.model"] != "" {
				t.Error("the model was not released from Magpie ownership")
			}
		})
	}
}

func TestClaudeDisconnectRejectsConcurrentEffortEdit(t *testing.T) {
	agent := connectedClaudeForDisconnect(t)
	target, err := edit.Target(agent.Path)
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
				t.Fatalf("effort removal did not start from an owned override: %q", before)
			}
			writeFile(t, path, userSettings)
		}
		return proposed
	}, agent.Disconnect)
	if userSettings == "" || !errors.Is(err, extension.ErrActionChanged) {
		t.Errorf("expected a stale plan after concurrent settings edit, injected=%v, err=%v", userSettings != "", err)
	}
	if got := readFile(agent.Path); got != userSettings {
		t.Errorf("changed user's settings: got %q, want %q", got, userSettings)
	}
	if got := stashLoad()["claude.model"]; got != "sonnet" {
		t.Errorf("model stash = %q, want sonnet", got)
	}
}

func connectedClaudeRouteForDisconnect(t *testing.T) *Agent {
	t.Helper()
	home := syncHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, path, `{
  "model": "opus",
  "env": {
    "ANTHROPIC_BASE_URL": "https://own.example",
    "ANTHROPIC_AUTH_TOKEN": "own-token",
    "ANTHROPIC_MODEL": "own-main",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "own-opus",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "own-sonnet",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "own-haiku",
    "CLAUDE_CODE_SUBAGENT_MODEL": "own-subagent"
  }
}
`)
	agent := claude(home)
	if err := agent.Connect(); err != nil {
		t.Fatal(err)
	}
	if !agent.Wired() {
		t.Fatal("Connect did not route Claude Code")
	}
	return agent
}

func TestClaudeDisconnectPreservesHandEditedRouteFields(t *testing.T) {
	agent := connectedClaudeRouteForDisconnect(t)
	if err := agent.Apply("fable", "relay/glm-4.6"); err != nil {
		t.Fatal(err)
	}
	if model, _ := edit.GetJSON(agent.Path, "model"); model == "haiku" {
		t.Fatal("the test must change the model alias written by Connect")
	}
	if err := edit.SetJSON(agent.Path,
		edit.KV{Path: "model", Value: "haiku"},
		edit.KV{Path: "env.ANTHROPIC_AUTH_TOKEN", Value: "my-new-key"},
		edit.KV{Path: "env.ANTHROPIC_DEFAULT_HAIKU_MODEL", Value: "my-new-haiku"},
		edit.KV{Path: "env.CLAUDE_CODE_SUBAGENT_MODEL", Value: "my-new-subagent"},
	); err != nil {
		t.Fatal(err)
	}
	if err := edit.DelJSON(agent.Path, "env.ANTHROPIC_DEFAULT_OPUS_MODEL"); err != nil {
		t.Fatal(err)
	}
	if !agent.Wired() {
		t.Fatal("the Fable tier must still point to the gateway before disconnect")
	}
	if err := agent.Disconnect(); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"model":                              "haiku",
		"env.ANTHROPIC_BASE_URL":             "https://own.example",
		"env.ANTHROPIC_AUTH_TOKEN":           "my-new-key",
		"env.ANTHROPIC_MODEL":                "own-main",
		"env.ANTHROPIC_DEFAULT_SONNET_MODEL": "own-sonnet",
		"env.ANTHROPIC_DEFAULT_HAIKU_MODEL":  "my-new-haiku",
		"env.CLAUDE_CODE_SUBAGENT_MODEL":     "my-new-subagent",
	} {
		if got, exists := edit.GetJSON(agent.Path, key); !exists || got != want {
			t.Errorf("%s = %q, present=%v; want %q", key, got, exists, want)
		}
	}
	if _, exists := edit.GetJSON(agent.Path, "env.ANTHROPIC_DEFAULT_OPUS_MODEL"); exists {
		t.Error("Disconnect restored the tier the user deleted")
	}
	if agent.Wired() || stashLoad()["claude.model"] != "" {
		t.Error("Disconnect left route ownership behind")
	}
}

func TestClaudeDisconnectRejectsConcurrentRouteRestore(t *testing.T) {
	agent := connectedClaudeRouteForDisconnect(t)
	target, err := edit.Target(agent.Path)
	if err != nil {
		t.Fatal(err)
	}
	var external string
	err = edit.Filtered(func(path string, proposed []byte) []byte {
		if path == target && external == "" && strings.Contains(string(proposed), `"https://own.example"`) {
			body, err := edit.PatchJSON([]byte(readFile(path)),
				[]edit.KV{{Path: "env.ANTHROPIC_DEFAULT_HAIKU_MODEL", Value: "my-replacement"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			external = string(body)
			writeFile(t, path, external)
		}
		return proposed
	}, agent.Disconnect)
	if external == "" || !errors.Is(err, extension.ErrActionChanged) {
		t.Errorf("expected a stale restoration, edit injected=%v, error=%v", external != "", err)
	}
	if readFile(agent.Path) != external {
		t.Error("Disconnect overwrote a concurrent settings edit")
	}
	for key, want := range map[string]string{
		"claude.model":               "opus",
		"claude.base_url":            "https://own.example",
		"claude.auth_token":          "own-token",
		"claude.env.ANTHROPIC_MODEL": "own-main",
	} {
		if got := stashLoad()[key]; got != want {
			t.Errorf("saved %s = %q, want %q", key, got, want)
		}
	}
	if t.Failed() {
		return
	}
	if err := agent.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if got, _ := edit.GetJSON(agent.Path, "env.ANTHROPIC_DEFAULT_HAIKU_MODEL"); got != "my-replacement" {
		t.Errorf("retry changed the user's tier to %q", got)
	}
	if got, _ := edit.GetJSON(agent.Path, "env.ANTHROPIC_AUTH_TOKEN"); got != "own-token" {
		t.Error("retry failed to restore the original key")
	}
	if agent.Wired() {
		t.Error("retry left Claude Code routed")
	}
}

func TestClaudeDisconnectKeepsEditedEndpointWithoutRestoringItsOldKey(t *testing.T) {
	agent := connectedClaudeRouteForDisconnect(t)
	const changed = "https://different.example"
	if err := edit.SetJSON(agent.Path, edit.KV{Path: "env.ANTHROPIC_BASE_URL", Value: changed}); err != nil {
		t.Fatal(err)
	}
	if !agent.Wired() {
		t.Fatal("Claude Code must still reference the routed model before disconnect")
	}
	if err := agent.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if got, _ := edit.GetJSON(agent.Path, "env.ANTHROPIC_BASE_URL"); got != changed {
		t.Errorf("Disconnect changed the user's endpoint to %q", got)
	}
	if _, exists := edit.GetJSON(agent.Path, "env.ANTHROPIC_AUTH_TOKEN"); exists {
		t.Error("Disconnect left a key intended for a different endpoint")
	}
}

func TestClaudeDisconnectRestoresRouteWithoutFieldClaims(t *testing.T) {
	agent := connectedClaudeRouteForDisconnect(t)
	forget("claude" + wireClaims)
	if err := agent.Disconnect(); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"model":                              "opus",
		"env.ANTHROPIC_BASE_URL":             "https://own.example",
		"env.ANTHROPIC_AUTH_TOKEN":           "own-token",
		"env.ANTHROPIC_DEFAULT_SONNET_MODEL": "own-sonnet",
	} {
		if got, exists := edit.GetJSON(agent.Path, key); !exists || got != want {
			t.Errorf("%s = %q, present=%v; want %q", key, got, exists, want)
		}
	}
	if agent.Wired() {
		t.Error("Disconnect left the earlier route wired")
	}
}
