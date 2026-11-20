package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
)

func TestPiMagpieTakeoverKeepsConcurrentEdits(t *testing.T) {
	for _, action := range []string{"sync", "pick"} {
		t.Run(action, func(t *testing.T) {
			client := pi(syncHome(t))
			models := filepath.Join(client.Dir, "models.json")
			if err := client.Apply("model", "magpie/relay/glm-4.6"); err != nil {
				t.Fatal(err)
			}
			catalog := readFile(models)
			const config = `{"packages":["npm:pi-magpie"],"defaultProvider":"magpie","defaultModel":"relay/old","enabledModels":["magpie/relay/old"]}`
			writeFile(t, client.Path, config)
			conflict := models
			user := `{"providers":{"magpie":{"baseUrl":"https://user.invalid/v1","apiKey":"user-set"}}}`
			operate := client.Sync
			if action == "pick" {
				conflict = client.Path
				user = strings.TrimSuffix(config, "}") + `,"theme":"dark"}`
				operate = func() error { return client.Pick("model", "magpie/relay/glm-4.6") }
			}
			target, err := edit.Target(conflict)
			if err != nil {
				t.Fatal(err)
			}
			edited := false
			err = edit.Filtered(func(path string, body []byte) []byte {
				if path == target && !edited {
					edited = true
					writeFile(t, path, user)
				}
				return body
			}, operate)
			if !edited || !errors.Is(err, extension.ErrActionChanged) || readFile(conflict) != user {
				t.Errorf("takeover lost the current file: edited=%t, err=%v, contents=%s", edited, err, readFile(conflict))
			}
			if action == "pick" && readFile(models) != catalog {
				t.Error("rejected pick removed the old catalog")
			}
			if _, err := os.Stat(models + ".before-pi-magpie"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("rejected takeover left a backup: %v", err)
			}
			if err := operate(); err != nil {
				t.Fatal(err)
			}
			if action == "sync" {
				if readFile(models) != user {
					t.Fatal("Sync removed the replacement provider on retry")
				}
			} else if readFile(models+".before-pi-magpie") != catalog ||
				!strings.Contains(readFile(client.Path), `"theme":"dark"`) ||
				strings.Contains(readFile(models), `"magpie"`) {
				t.Fatal("the accepted pick did not retain the original catalog and the new settings")
			}
		})
	}
}

// With the pi-magpie extension installed, magpie's own providers.magpie
// block in models.json is taken out (a copy kept), the user's other
// providers stay, and a block that is not magpie's stays too.
func TestPiMagpieExtensionTakesOverTheBlock(t *testing.T) {
	home := syncHome(t)
	dir := filepath.Join(home, ".pi", "agent")
	os.MkdirAll(dir, 0o755)
	models := filepath.Join(dir, "models.json")
	settings := filepath.Join(dir, "settings.json")
	os.WriteFile(settings, []byte(`{"packages":[]}`), 0o644)
	os.WriteFile(models, []byte(`{"providers":{"mine":{"baseUrl":"http://x/v1"}}}`), 0o644)
	if err := provider.Save(provider.Provider{ID: "p", Name: "P", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"m"}}); err != nil {
		t.Fatal(err)
	}
	// plain Pi: the block is written
	if err := pi(home).Field("model").Set("magpie/p/m"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(models), `"magpie"`) {
		t.Fatalf("without the extension the block is written: %s", readFile(models))
	}
	// the extension installed: the next sync drops it
	os.WriteFile(settings, []byte(`{"packages":["npm:pi-magpie"],"defaultProvider":"magpie"}`), 0o644)
	if err := pi(home).Sync(); err != nil {
		t.Fatal(err)
	}
	got := readFile(models)
	if strings.Contains(got, `"magpie"`) || !strings.Contains(got, `"mine"`) {
		t.Fatalf("block not dropped or mine lost: %s", got)
	}
	if _, err := os.Stat(models + ".before-pi-magpie"); err != nil {
		t.Fatalf("no backup: %v", err)
	}
	if msg := pi(home).Check(); msg != "" {
		t.Fatalf("check: %s", msg)
	}
	// a block the user wrote (another address) is left alone
	os.WriteFile(models, []byte(`{"providers":{"magpie":{"baseUrl":"http://example.com/v1","apiKey":"theirs"}}}`), 0o644)
	if err := pi(home).Sync(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(models), "example.com") {
		t.Fatalf("user's block removed: %s", readFile(models))
	}
}
