package agent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func TestClaudeConnectPreservesConcurrentRouteEdit(t *testing.T) {
	home := syncHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, path, "\xef\xbb\xbf"+strings.ReplaceAll(`{
  // User preferences
  "model": "sonnet",
  "env": {"ANTHROPIC_MODEL": "claude-user-model", "OTHER_ENV": "keep"}
}
`, "\n", "\r\n"))
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := access.Update("add-key", access.Change{Name: "claude-route-test"})
	if err != nil {
		t.Fatal(err)
	}
	agent := claude(home)
	target, err := edit.Target(path)
	if err != nil {
		t.Fatal(err)
	}
	var external string
	err = edit.Filtered(func(name string, proposed []byte) []byte {
		if name == target && external == "" && strings.Contains(string(proposed), `"ANTHROPIC_BASE_URL"`) {
			body, err := edit.PatchJSON([]byte(readFile(name)), []edit.KV{
				{Path: "env.ANTHROPIC_AUTH_TOKEN", Value: key},
				{Path: "env." + claudeOutputEnv, Value: "12345"},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			external = string(body)
			writeFile(t, name, external)
		}
		return proposed
	}, agent.Connect)
	if external == "" || !errors.Is(err, extension.ErrActionChanged) {
		t.Errorf("expected a stale route plan, edit injected=%v, error=%v", external != "", err)
	}
	if readFile(path) != external {
		t.Error("Connect overwrote the user's concurrent settings write")
	}
	if value, ok := edit.GetJSON(path, "env.ANTHROPIC_MODEL"); !ok || value != "claude-user-model" {
		t.Error("a rejected route plan removed the user's original model override")
	}
	if saved := stashLoad(); saved["claude.main"] != "" || saved["claude.output_tokens"] != "" {
		t.Error("a rejected route plan claimed model or output settings")
	}
	if t.Failed() {
		return
	}
	if err := agent.Connect(); err != nil {
		t.Fatal(err)
	}
	if !agent.Wired() || agent.Field("model").Get() != "relay/glm-4.6" {
		t.Fatal("Connect retry did not select the relay model")
	}
	if value, _ := edit.GetJSON(path, "env.ANTHROPIC_AUTH_TOKEN"); value != key {
		t.Error("Connect retry changed the user's gateway key")
	}
	if value, _ := edit.GetJSON(path, "env."+claudeOutputEnv); value != "12345" {
		t.Error("Connect retry changed the user's output limit")
	}
	if _, ok := edit.GetJSON(path, "env.ANTHROPIC_MODEL"); ok {
		t.Error("Connect left an override that takes precedence over /model")
	}
	got := readFile(path)
	if !strings.HasPrefix(got, "\xef\xbb\xbf") || !strings.Contains(got, "  // User preferences\r\n") ||
		strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Error("Connect changed the settings file's BOM, comments or line endings")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("settings mode = %o, want 0600", info.Mode().Perm())
	}
}
