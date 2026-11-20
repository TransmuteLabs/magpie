package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func TestCodexDesktopReferenceRejectsConcurrentEdit(t *testing.T) {
	for _, scenario := range []string{"catalog-removed", "config-edited"} {
		t.Run(scenario, func(t *testing.T) {
			home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, "model = \"gpt-5.5\"\n")
			app := false
			codexAppReads(t, &app)
			list := `{"models":[{"slug":"gpt-6-sol"}]}`
			deskListIs(t, &list)
			cx := codex(home)
			if ok, err := cx.Join(); err != nil || !ok {
				t.Fatalf("join: %v %v", ok, err)
			}
			desk := filepath.Join(cx.Dir, "magpie-desktop-models.json")
			config, err := edit.Target(cx.Path)
			if err != nil {
				t.Fatal(err)
			}
			app = true
			wantConfig := read()
			changed := false
			err = edit.Filtered(func(path string, body []byte) []byte {
				if path == config && strings.Contains(string(body), "magpie-desktop-models.json") && !changed {
					if scenario == "catalog-removed" {
						if err := os.Remove(desk); err != nil {
							t.Fatal(err)
						}
					} else {
						wantConfig = read() + "\n# external settings edit\n"
						writeFile(t, config, wantConfig)
					}
					changed = true
				}
				return body
			}, cx.Sync)
			if !changed || !errors.Is(err, extension.ErrActionChanged) || read() != wantConfig || !cx.DeskAt().IsZero() {
				t.Fatalf("sync replaced a concurrent edit or published the catalog reference: changed=%v, sync=%v, config=%q", changed, err, read())
			}
			if scenario == "catalog-removed" {
				if _, err := os.Stat(desk); !os.IsNotExist(err) {
					t.Fatalf("sync restored the removed desktop catalog: %v", err)
				}
			}
			if err := cx.Sync(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(read(), "magpie-desktop-models.json") || readFile(desk) != list || cx.DeskAt().IsZero() {
				t.Fatal("next sync did not publish a valid desktop catalog reference")
			}
		})
	}
}

func TestCodexDesktopKeepsNewUserReferenceWhenListIsUnknown(t *testing.T) {
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, "model = \"gpt-5.5\"\n")
	app := true
	codexAppReads(t, &app)
	list := `{"models":[{"slug":"gpt-6-sol"}]}`
	deskListIs(t, &list)
	cx := codex(home)
	if ok, err := cx.Join(); err != nil || !ok {
		t.Fatalf("join: %v %v", ok, err)
	}
	desk := filepath.Join(cx.Dir, "magpie-desktop-models.json")
	if err := os.Remove(desk); err != nil {
		t.Fatal(err)
	}
	var want string
	CodexDesktopList = func() ([]byte, bool) {
		config := read()
		for _, line := range strings.Split(config, "\n") {
			if strings.HasPrefix(line, "model_catalog_json = ") {
				want = strings.Replace(config, line, `model_catalog_json = "mine.json"`, 1)
				break
			}
		}
		if want == "" {
			t.Fatal("missing model_catalog_json before concurrent edit")
		}
		writeFile(t, filepath.Join(cx.Dir, "mine.json"), `{"models":[]}`)
		writeFile(t, cx.Path, want)
		return nil, false
	}
	if err := cx.Sync(); !errors.Is(err, extension.ErrActionChanged) || want == "" || read() != want {
		t.Fatalf("sync dropped the user's replacement reference: changed=%v, err=%v, config=%q", want != "", err, read())
	}
	if _, err := os.Stat(desk); !os.IsNotExist(err) {
		t.Fatalf("sync restored the removed desktop catalog: %v", err)
	}
}

func TestCodexDesktopCatalogConflictKeepsConcurrentConfigEdit(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		named bool
	}{
		{name: "named", named: true},
		{name: "unnamed"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, "model = \"gpt-5.5\"\n")
			app := scenario.named
			codexAppReads(t, &app)
			initial := `{"models":[{"slug":"first"}]}`
			deskListIs(t, &initial)
			cx := codex(home)
			if ok, err := cx.Join(); err != nil || !ok {
				t.Fatalf("join: %t, %v", ok, err)
			}
			app = true
			desk := filepath.Join(cx.Dir, "magpie-desktop-models.json")
			target, err := edit.Target(desk)
			if err != nil {
				t.Fatal(err)
			}
			const deskEdit = `{"models":[{"slug":"external"}]}`
			var configEdit string
			CodexDesktopList = func() ([]byte, bool) {
				configEdit = read() + "\n# user edited config during fetch\n"
				writeFile(t, cx.Path, configEdit)
				return []byte(`{"models":[{"slug":"generated"}]}`), true
			}
			edited := false
			err = edit.Filtered(func(path string, body []byte) []byte {
				if path == target && !edited {
					edited = true
					writeFile(t, path, deskEdit)
				}
				return body
			}, cx.Sync)
			if configEdit == "" || !edited || !errors.Is(err, extension.ErrActionChanged) {
				t.Fatalf("sync did not reject the stale desktop catalog: edited=%t err=%v", edited, err)
			}
			if read() != configEdit || readFile(desk) != deskEdit {
				t.Fatal("sync rollback erased a concurrent edit to the config or desktop catalog")
			}
		})
	}
}
