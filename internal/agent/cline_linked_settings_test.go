package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/gateway"
)

func TestClineSyncUsesCurrentSettingsDuringCatalogHold(t *testing.T) {
	home := syncHome(t)
	t.Setenv("CLINE_DIR", "")
	t.Setenv("CLINE_DATA_DIR", "")
	a := cline(home)
	models := filepath.Join(filepath.Dir(a.Path), "models.json")
	const slot = "providers." + clineSlot
	const modelKey = slot + ".settings.model"
	const override = slot + ".models.relay/glm-4\\.6.temperature"
	old := `{"lastUsedProvider":"openai-compatible","providers":{"openai-compatible":{"settings":{"apiKey":"` +
		gateway.Token + `","model":"relay/old","baseUrl":"` + gatewayV1() + `"}}}}`
	catalog := `{"providers":{"openai-compatible":{"provider":{"defaultModelId":"relay/old"},"models":{"relay/glm-4.6":{"temperature":0.1}}}}}`
	writeFile(t, a.Path, old)
	writeFile(t, models, catalog)
	settled := time.Now().Add(-time.Minute)
	for _, path := range []string{a.Path, models} {
		if err := os.Chtimes(path, settled, settled); err != nil {
			t.Fatal(err)
		}
	}
	release := filememo.Hold()
	defer release()
	if model, _ := edit.GetJSON(a.Path, modelKey); model != "relay/old" {
		t.Fatal("provider fixture was not read", model)
	}
	if temperature, _ := edit.GetJSON(models, override); temperature != "0.1" {
		t.Fatal("catalog fixture was not read", temperature)
	}
	writeFile(t, a.Path, strings.Replace(old, "relay/old", "relay/glm-4.6", 1))
	writeFile(t, models, strings.Replace(catalog, "0.1", "0.3", 1))
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{slot + ".provider.defaultModelId": "relay/glm-4.6", override: "0.3"} {
		if value, _ := edit.GetJSON(models, key); value != want {
			t.Errorf("Sync restored a stale value at %s: got %q, want %q", key, value, want)
		}
	}
}

func TestClineSyncRejectsMixedSettingsSnapshots(t *testing.T) {
	for _, concurrent := range []string{"providers.json", "secrets.json"} {
		t.Run(concurrent, func(t *testing.T) {
			home := syncHome(t)
			t.Setenv("CLINE_DIR", "")
			t.Setenv("CLINE_DATA_DIR", "")
			a := cline(home)
			data := filepath.Dir(filepath.Dir(a.Path))
			models := filepath.Join(data, "settings", "models.json")
			state := filepath.Join(data, "globalState.json")
			secrets := filepath.Join(data, "secrets.json")
			const oldURL = "http://127.0.0.1:1/v1"
			const slot = "providers." + clineSlot
			before := map[string]string{
				a.Path: `{"lastUsedProvider":"openai-compatible","providers":{"openai-compatible":{"settings":{"apiKey":"` +
					gateway.Token + `","model":"relay/glm-4.6","baseUrl":"` + oldURL + `","reasoning":{"effort":"high"}}}}}`,
				models:  `{"providers":{"openai-compatible":{"provider":{"baseUrl":"` + oldURL + `","modelsSourceUrl":"https://local.invalid/models"},"models":{"relay/glm-4.6":{"temperature":0.3}}}}}`,
				state:   `{"openAiBaseUrl":"` + oldURL + `","telemetrySetting":"disabled"}`,
				secrets: `{"openAiApiKey":"` + gateway.Token + `"}`,
			}
			for path, body := range before {
				writeFile(t, path, body)
				before[path] = readFile(path)
			}
			changed := a.Path
			if concurrent == "secrets.json" {
				changed = secrets
			}
			user := strings.Replace(before[changed], gateway.Token, "user-replaced-the-key", 1)
			target, err := edit.Target(models)
			if err != nil {
				t.Fatal(err)
			}
			edited := false
			err = edit.Filtered(func(path string, body []byte) []byte {
				if path == target && !edited {
					edited = true
					writeFile(t, changed, user)
				}
				return body
			}, a.Sync)
			if !edited || !errors.Is(err, extension.ErrActionChanged) {
				t.Errorf("Sync accepted mixed settings snapshots: edited=%t, error=%v", edited, err)
			}
			for path, want := range before {
				if path == changed {
					want = user
				}
				if readFile(path) != want {
					t.Errorf("rejected Sync failed to preserve %s", filepath.Base(path))
				}
			}
			if err := a.Sync(); err != nil {
				t.Fatal(err)
			}
			if concurrent == "providers.json" {
				if readFile(a.Path) != user || readFile(models) != before[models] || readFile(state) != before[state] {
					t.Fatal("retry changed the settings of a disconnected agent")
				}
				return
			}
			for path, field := range map[string]string{a.Path: slot + ".settings.baseUrl", models: slot + ".provider.baseUrl"} {
				if value, _ := edit.GetJSON(path, field); value != gatewayV1() {
					t.Errorf("retry did not repair %s: %q", filepath.Base(path), value)
				}
			}
			if readFile(secrets) != user || readFile(state) != before[state] {
				t.Fatal("retry changed VS Code settings governed by the user's new key")
			}
			for field, want := range map[string]string{
				slot + ".provider.modelsSourceUrl":           "https://local.invalid/models",
				slot + ".models.relay/glm-4\\.6.temperature": "0.3",
				slot + ".provider.defaultModelId":            "relay/glm-4.6",
			} {
				if value, _ := edit.GetJSON(models, field); value != want {
					t.Errorf("%s: got %q, want %q", field, value, want)
				}
			}
		})
	}
}
