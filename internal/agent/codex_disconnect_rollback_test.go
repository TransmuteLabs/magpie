package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
)

func TestCodexDisconnectKeepsStashWhenDesktopCleanupFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX directory permissions")
	}
	const mirror = "\n[model_providers.cc-switch-3]\nname = \"OpenAI\"\nrequires_openai_auth = true\nwire_api = \"responses\"\n"
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, ccSwitchCodex+mirror)
	cx := codex(home)
	if err := cx.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetTOMLKey(cx.Path, "agents", "default_subagent_model", "fake/m1"); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Dir(cx.Path)
	target := filepath.Join(t.TempDir(), "config.toml")
	if err := os.Rename(cx.Path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, cx.Path); err != nil {
		t.Fatal(err)
	}
	const comment = "\n# Codex user settings\n"
	writeFile(t, target, read()+comment)
	desktop := filepath.Join(dir, "magpie-desktop-models.json")
	writeFile(t, desktop, `{"models":[]}`)
	probe := filepath.Join(dir, "unlink-probe")
	writeFile(t, probe, "")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := os.Remove(probe); !errors.Is(err, fs.ErrPermission) {
		t.Skipf("test requires denied unlink in a read-only directory, got %v", err)
	}

	at := here(home)
	stash(map[string]string{at.key("codex.desk_at"): "1"})
	keys := []string{at.key("codex.tables"), at.key("codex.mirrors"), at.key("codex.model"),
		at.key("codex.provider"), at.key("codex.effort"), at.key("codex.desk_at")}
	saved := peek(keys...)
	for _, key := range keys {
		if saved[key] == "" {
			t.Fatalf("missing saved setting %q", key)
		}
	}
	routed := read()
	var removal *os.PathError
	if err := cx.Disconnect(); !errors.As(err, &removal) || removal.Op != "remove" ||
		removal.Path != desktop || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("expected a late desktop catalog removal error, got %v", err)
	}
	if got := read(); got != routed || !cx.Wired() {
		t.Errorf("failed Disconnect did not restore the routed config:\n%s", got)
	}
	for _, key := range keys {
		if got := stashLoad()[key]; got != saved[key] {
			t.Errorf("failed Disconnect lost saved setting %q: got %q, want %q", key, got, saved[key])
		}
	}
	if t.Failed() {
		return
	}

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cx.Disconnect(); err != nil {
		t.Fatal(err)
	}
	custom, err := edit.GetTOMLTable(cx.Path, "model_providers.custom")
	if err != nil || custom["base_url"] != "https://relay.example/v1" {
		t.Errorf("retry lost the original relay URL: %q (%v)", custom["base_url"], err)
	}
	mirrored, err := edit.GetTOMLTable(cx.Path, "model_providers.cc-switch-3")
	if err != nil || mirrored["base_url"] != "" {
		t.Errorf("retry kept the mirror on Magpie: %q (%v)", mirrored["base_url"], err)
	}
	if got := read(); cx.Wired() || strings.Contains(got, "fake/m1") ||
		!strings.Contains(got, `model = "gpt-5.4"`) || !strings.HasSuffix(got, comment) {
		t.Errorf("retry did not restore the original route and preserve the user's config:\n%s", got)
	}
	for _, key := range keys {
		if got := stashLoad()[key]; got != "" {
			t.Errorf("successful Disconnect kept %q: %q", key, got)
		}
	}
}

func TestCodexDisconnectKeepsNewerStashValues(t *testing.T) {
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, ccSwitchCodex)
	cx := codex(home)
	if err := cx.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	at := here(home)
	newer := map[string]string{
		at.key("codex.model"):    "newer-native-model",
		at.key("codex.provider"): "newer-provider",
		at.key("codex.tables"):   `{"custom":"https://newer.example/v1"}`,
	}
	target, err := edit.Target(cx.Path)
	if err != nil {
		t.Fatal(err)
	}
	injected := false
	err = edit.Filtered(func(path string, proposed []byte) []byte {
		if path == target && !injected && strings.Contains(string(proposed), `model = "gpt-5.4"`) {
			injected = true
			stash(newer)
		}
		return proposed
	}, cx.Disconnect)
	if err != nil || !injected || cx.Wired() {
		t.Fatalf("Disconnect did not restore the original config: injected=%t error=%v config=%q", injected, err, read())
	}
	for key, want := range newer {
		if got := stashLoad()[key]; got != want {
			t.Errorf("Disconnect deleted newer stash value %q: got %q, want %q", key, got, want)
		}
	}
	custom, err := edit.GetTOMLTable(cx.Path, "model_providers.custom")
	if err != nil || custom["base_url"] != "https://relay.example/v1" {
		t.Errorf("Disconnect used the newer saved relay URL: got %q (%v)", custom["base_url"], err)
	}
}
