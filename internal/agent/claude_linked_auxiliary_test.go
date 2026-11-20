package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func TestClaudeAuxiliarySettingsRejectConcurrentEdits(t *testing.T) {
	for _, field := range []struct{ name, claim string }{
		{"auto_mode", "claude.auto_mode_model"}, {"advisor", "claude.advisor_env"},
	} {
		for _, operation := range []string{"select", "clear"} {
			t.Run(field.name+"/"+operation, func(t *testing.T) {
				agent, path, _ := claudeAutoModeHome(t, `{"theme":"dark"}`)
				if err := agent.Apply("model", "relay/deepseek-pro"); err != nil {
					t.Fatal(err)
				}
				pick := "relay/claude-haiku-4-5"
				if operation == "clear" {
					if err := agent.Apply(field.name, pick); err != nil {
						t.Fatal(err)
					}
					pick = ""
				}
				before := stashLoad()[field.claim]
				target, err := edit.Target(path)
				if err != nil {
					t.Fatal(err)
				}
				var external string
				err = edit.Filtered(func(name string, proposed []byte) []byte {
					if name == target && external == "" {
						body, err := edit.PatchJSON([]byte(readFile(name)), []edit.KV{{Path: "theme", Value: "light"}}, nil)
						if err != nil {
							t.Fatal(err)
						}
						external = string(body)
						writeFile(t, name, external)
					}
					return proposed
				}, func() error { return agent.Apply(field.name, pick) })
				if external == "" || !errors.Is(err, extension.ErrActionChanged) || readFile(path) != external {
					t.Errorf("Apply = %v, concurrent settings preserved = %t", err, external != "" && readFile(path) == external)
				}
				if got := stashLoad()[field.claim]; got != before {
					t.Errorf("rejected %s changed %s claim from %q to %q", operation, field.name, before, got)
				}
			})
		}
	}
}

func TestClaudeConnectAdoptsClassifierOnlyWithRoute(t *testing.T) {
	const classifier = "claude-sonnet-4-6"
	agent, path, env := claudeAutoModeHome(t, `{"env":{"CLAUDE_CODE_AUTO_MODE_MODEL":"claude-sonnet-4-6"}}`)
	target, err := edit.Target(path)
	if err != nil {
		t.Fatal(err)
	}
	var external string
	var earlyCleanup, routeContainsClassifier bool
	err = edit.Filtered(func(name string, proposed []byte) []byte {
		if name != target || external != "" {
			return proposed
		}
		body := string(proposed)
		if !strings.Contains(body, `"ANTHROPIC_BASE_URL"`) {
			earlyCleanup = earlyCleanup || !strings.Contains(body, claudeAutoModeEnv)
			return proposed
		}
		routeContainsClassifier = strings.Contains(body, claudeAutoModeEnv)
		next, err := edit.PatchJSON([]byte(readFile(name)), []edit.KV{{Path: "theme", Value: "light"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		external = string(next)
		writeFile(t, name, external)
		return proposed
	}, func() error { return agent.Apply("model", "relay/deepseek-pro") })
	if external == "" || !errors.Is(err, extension.ErrActionChanged) || readFile(path) != external {
		t.Errorf("Apply = %v, concurrent settings preserved = %t", err, external != "" && readFile(path) == external)
	}
	originalKey := "claude.env." + claudeAutoModeEnv
	if earlyCleanup || routeContainsClassifier || env(claudeAutoModeEnv) != classifier || stashLoad()[originalKey] != "" {
		t.Errorf("classifier taken before successful route publication: early cleanup = %t, route included classifier = %t, value = %q, stash = %q",
			earlyCleanup, routeContainsClassifier, env(claudeAutoModeEnv), stashLoad()[originalKey])
	}
	if t.Failed() {
		return
	}
	if err := agent.Apply("model", "relay/deepseek-pro"); err != nil {
		t.Fatal(err)
	}
	if env(claudeAutoModeEnv) != "" || stashLoad()[originalKey] != classifier {
		t.Fatal("successful Connect did not preserve the original classifier for restore")
	}
	if err := agent.Disconnect(); err != nil || env(claudeAutoModeEnv) != classifier {
		t.Fatalf("Disconnect = %v, classifier = %q", err, env(claudeAutoModeEnv))
	}
}

func TestClaudeSyncKeepsClassifierOnConcurrentEndpointChange(t *testing.T) {
	const classifier = "relay/deepseek-pro"
	agent, path, env := claudeAutoModeHome(t, `{"env":{"CLAUDE_CODE_AUTO_MODE_MODEL":"relay/deepseek-pro"}}`)
	target, err := edit.Target(path)
	if err != nil {
		t.Fatal(err)
	}
	var external string
	err = edit.Filtered(func(name string, proposed []byte) []byte {
		if name == target && external == "" && !strings.Contains(string(proposed), claudeAutoModeEnv) {
			body, err := edit.PatchJSON([]byte(readFile(name)), []edit.KV{
				{Path: "env.ANTHROPIC_BASE_URL", Value: "https://private-relay.test/v1"},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			external = string(body)
			writeFile(t, name, external)
		}
		return proposed
	}, agent.Sync)
	if external == "" || !errors.Is(err, extension.ErrActionChanged) || readFile(path) != external {
		t.Errorf("Sync = %v, concurrent settings preserved = %t", err, external != "" && readFile(path) == external)
	}
	if t.Failed() {
		return
	}
	if err := agent.Sync(); err != nil || env(claudeAutoModeEnv) != classifier {
		t.Fatalf("Sync against a custom endpoint = %v, classifier = %q", err, env(claudeAutoModeEnv))
	}
}
