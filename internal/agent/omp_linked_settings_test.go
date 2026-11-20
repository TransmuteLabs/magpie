package agent

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func TestOmpProfileConnectSyncDisconnect(t *testing.T) {
	home := syncHome(t)
	dir := filepath.Join(home, ".omp", "profiles", "work", "agent")
	config := filepath.Join(dir, "config.yaml")
	models := filepath.Join(dir, "models.yaml")
	const own = "\xef\xbb\xbf# profile settings\r\nmodelRoles:\r\n  default: anthropic/claude-opus-5 # mine\r\nenabledModels: [anthropic/claude-opus-5]\r\n"
	writeFile(t, config, own)
	writeFile(t, models, "providers:\n  mine:\n    api: openai-completions\n")
	profiles := ompProfiles(home)
	if len(profiles) != 1 || profiles[0].ID != "omp#work" || profiles[0].Path != config {
		t.Fatalf("profiles = %v", profiles)
	}
	a := profiles[0]
	if err := a.Connect(); err != nil {
		t.Fatal(err)
	}
	if !a.Wired() || a.Field("model").Get() != "magpie/relay/glm-4.6" || !strings.Contains(readFile(config), "magpie/**") {
		t.Fatalf("wired = %t, config = %q", a.Wired(), readFile(config))
	}
	if got := readFile(config); !strings.HasPrefix(got, "\xef\xbb\xbf# profile settings\r\n") || strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Fatalf("connect changed config encoding: %q", got)
	}
	if err := a.Apply("protocol", "openai-responses"); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if got, _ := edit.GetYAML(models, "providers.magpie.api"); got != "openai-responses" {
		t.Fatalf("provider api = %q", got)
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if a.Wired() || a.Field("model").Get() != "anthropic/claude-opus-5" || strings.Contains(readFile(config), "magpie/") ||
		!strings.Contains(readFile(config), "# mine") || strings.Contains(readFile(models), "  magpie:") ||
		!strings.Contains(readFile(models), "  mine:") {
		t.Fatalf("wired = %t, config = %q, models = %q", a.Wired(), readFile(config), readFile(models))
	}
}

func TestOmpApplyRetainsConcurrentSettingsEdit(t *testing.T) {
	a := omp(syncHome(t))
	models := filepath.Join(a.Dir, "models.yml")
	const config = "theme: dark\nmodelRoles:\n  default: anthropic/claude-opus-5\n"
	const user = "theme: light\nmodelRoles:\n  default: anthropic/claude-opus-5\n"
	const providers = "providers:\n  mine:\n    api: openai-completions\n"
	writeFile(t, a.Path, config)
	writeFile(t, models, providers)
	configTarget, err := edit.Target(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	edited := false
	err = edit.Filtered(func(path string, data []byte) []byte {
		if path == configTarget && !edited {
			writeFile(t, a.Path, user)
			edited = true
		}
		return data
	}, func() error { return a.Apply("model", "magpie/relay/glm-4.6") })
	if !edited || !errors.Is(err, extension.ErrActionChanged) || readFile(a.Path) != user || readFile(models) != providers {
		t.Fatalf("edited = %t, error = %v, config = %q, models = %q", edited, err, readFile(a.Path), readFile(models))
	}
}

func TestOmpSyncRetainsConcurrentCatalogEdit(t *testing.T) {
	a := omp(syncHome(t))
	models := filepath.Join(a.Dir, "models.yaml")
	const catalog = "providers:\n  magpie:\n    baseUrl: http://127.0.0.1:1/v1\n    headers:\n      Extra: old\n"
	const user = "providers:\n  magpie:\n    baseUrl: http://127.0.0.1:1/v1\n    headers:\n      Extra: new\n"
	writeFile(t, models, catalog)
	modelsTarget, err := edit.Target(models)
	if err != nil {
		t.Fatal(err)
	}
	edited := false
	err = edit.Filtered(func(path string, data []byte) []byte {
		if path == modelsTarget && !edited {
			writeFile(t, models, user)
			edited = true
		}
		return data
	}, a.Sync)
	if !edited || !errors.Is(err, extension.ErrActionChanged) || readFile(models) != user {
		t.Fatalf("edited = %t, error = %v, models = %q", edited, err, readFile(models))
	}
}
