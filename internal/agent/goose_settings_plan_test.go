package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func TestGooseApplyPreservesCatalogWhenSettingsAreInvalid(t *testing.T) {
	home := syncHome(t)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	goose := goose(home, filepath.Join(home, ".config"))
	const settings = "providers: [\n"
	const catalog = `{"display_name":"My gateway","timeout_seconds":900}`
	catalogPath := gooseProviderPath(goose.Path)
	writeFile(t, goose.Path, settings)
	writeFile(t, catalogPath, catalog)

	if err := goose.Apply("model", "magpie/relay/glm-4.6"); err == nil {
		t.Fatal("Apply accepted invalid Goose settings")
	}
	if got := readFile(goose.Path); got != settings {
		t.Errorf("Apply changed invalid settings: %q", got)
	}
	if got := readFile(catalogPath); got != catalog {
		t.Error("Apply published the catalog despite invalid settings")
	}
}

func TestGooseApplyRollsBackCatalogOnConcurrentSettings(t *testing.T) {
	home := syncHome(t)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	agent := goose(home, filepath.Join(home, ".config"))
	const original = "GOOSE_PROVIDER: anthropic\nGOOSE_MODEL: claude-sonnet-4-5\n"
	const edited = original + "GOOSE_MODE: auto\n"
	const catalog = `{"display_name":"My gateway","timeout_seconds":900}`
	catalogPath := gooseProviderPath(agent.Path)
	writeFile(t, agent.Path, original)
	writeFile(t, catalogPath, catalog)
	target, err := edit.Target(agent.Path)
	if err != nil {
		t.Fatal(err)
	}
	var changed bool
	err = edit.Filtered(func(path string, body []byte) []byte {
		if path == target && !changed {
			writeFile(t, target, edited)
			changed = true
		}
		return body
	}, func() error { return agent.Apply("model", "magpie/relay/glm-4.6") })

	if !changed || !errors.Is(err, extension.ErrActionChanged) {
		t.Errorf("Apply did not reject concurrent settings: changed=%t, err=%v", changed, err)
	}
	if got := readFile(agent.Path); got != edited {
		t.Errorf("Apply overwrote concurrent settings: %q", got)
	}
	if got := readFile(catalogPath); got != catalog {
		t.Error("Apply retained a catalog from a rejected update")
	}
}

func TestGooseSyncKeepsConcurrentProviderEdits(t *testing.T) {
	home := syncHome(t)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	agent := goose(home, filepath.Join(home, ".config"))
	const catalog = `{"display_name":"My gateway","base_url":"http://elsewhere/v1","timeout_seconds":100}`
	const edited = `{"display_name":"My gateway","base_url":"http://elsewhere/v1","timeout_seconds":900}`
	catalogPath := gooseProviderPath(agent.Path)
	writeFile(t, catalogPath, catalog)
	target, err := edit.Target(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	var changed bool
	err = edit.Filtered(func(path string, body []byte) []byte {
		if path == target && !changed {
			writeFile(t, target, edited)
			changed = true
		}
		return body
	}, agent.Sync)

	if !changed || !errors.Is(err, extension.ErrActionChanged) {
		t.Errorf("Sync did not reject a concurrent catalog change: changed=%t, err=%v", changed, err)
	}
	if got := readFile(catalogPath); got != edited {
		t.Errorf("Sync overwrote concurrent settings: %q", got)
	}
}

func TestGooseApplyPreservesBOMAndCatalogSettings(t *testing.T) {
	home := syncHome(t)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	agent := goose(home, filepath.Join(home, ".config"))
	const bom = "\ufeff"
	catalogPath := gooseProviderPath(agent.Path)
	writeFile(t, agent.Path, bom+"GOOSE_PROVIDER: anthropic\r\nGOOSE_MODEL: claude-sonnet-4-5\r\nGOOSE_MODE: auto\r\n")
	writeFile(t, catalogPath, bom+`{"display_name":"My gateway","timeout_seconds":900}`)

	if err := agent.Apply("model", "magpie/relay/glm-4.6"); err != nil {
		t.Fatal(err)
	}
	settings := readFile(agent.Path)
	if settings != bom+"GOOSE_PROVIDER: magpie\r\nGOOSE_MODEL: relay/glm-4.6\r\nGOOSE_MODE: auto\r\n" {
		t.Errorf("Apply changed the BOM, line endings or unrelated setting: %q", settings)
	}
	body := readFile(catalogPath)
	if !strings.HasPrefix(body, bom) {
		t.Fatal("Apply discarded the catalog's BOM")
	}
	var catalog struct {
		DisplayName string `json:"display_name"`
		Timeout     int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(body, bom)), &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.DisplayName != "My gateway" || catalog.Timeout != 900 {
		t.Errorf("Apply discarded user-owned catalog settings: %+v", catalog)
	}
}

func TestGooseResetPreservesCatalogLinks(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		link     func(string, string) error
		dangling bool
	}{
		{name: "symlink", link: os.Symlink},
		{name: "hardlink", link: os.Link},
		{name: "dangling symlink", link: os.Symlink, dangling: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			home := syncHome(t)
			t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
			agent := goose(home, filepath.Join(home, ".config"))
			writeFile(t, agent.Path, "active_provider: magpie\nproviders:\n  magpie:\n    model: relay/glm-4.6\n  anthropic:\n    model: claude-sonnet-4-5\n")
			catalogPath := gooseProviderPath(agent.Path)
			referent := filepath.Join(filepath.Dir(agent.Path), "catalog.json")
			if !fixture.dangling {
				writeFile(t, referent, `{"display_name":"My gateway"}`)
			}
			if err := os.MkdirAll(filepath.Dir(catalogPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := fixture.link(referent, catalogPath); err != nil {
				t.Skipf("catalog link unavailable: %v", err)
			}
			before, err := os.Lstat(catalogPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := agent.Apply("model", ""); err != nil {
				t.Fatal(err)
			}
			if after, err := os.Lstat(catalogPath); err != nil || !os.SameFile(before, after) {
				t.Fatalf("reset replaced the catalog link: %v", err)
			}
			if fixture.dangling {
				if _, err := os.Stat(referent); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("reset changed the dangling link's referent: %v", err)
				}
			} else if body, err := os.ReadFile(referent); err != nil || len(body) != 0 {
				t.Errorf("reset did not clear the linked catalog: %q, %v", body, err)
			}
			if model, _ := edit.GetYAML(agent.Path, "providers.anthropic.model"); model != "claude-sonnet-4-5" {
				t.Errorf("reset changed another provider's model: %q", model)
			}
		})
	}
}
