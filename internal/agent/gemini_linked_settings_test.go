package agent

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func TestGeminiRouteRejectsMalformedSettingsBeforeChangingEnv(t *testing.T) {
	home, _ := codexHome(t, "", "")
	target := gemini(home)
	env := filepath.Join(target.Dir, ".env")
	const malformed = `{"security": [`
	const originalEnv = "GEMINI_API_KEY=own-key\nLOCAL_VALUE=keep\n"
	writeFile(t, target.Path, malformed)
	writeFile(t, env, originalEnv)
	stash(map[string]string{"gemini.api_key": "saved-key"})

	if err := target.Apply("model", "fake/m1"); err == nil {
		t.Fatal("Apply accepted malformed settings")
	}
	if got := readFile(env); got != originalEnv {
		t.Fatal("failed Apply changed Gemini's environment")
	}
	if readFile(target.Path) != malformed || peek("gemini.api_key")["gemini.api_key"] != "saved-key" {
		t.Fatal("failed Apply changed Gemini's settings or restoration state")
	}
}

func TestGeminiRouteRollsBackEnvWhenSettingsChange(t *testing.T) {
	home, _ := codexHome(t, "", "")
	target := gemini(home)
	env := filepath.Join(target.Dir, ".env")
	const originalEnv = "GEMINI_API_KEY=own-key\nLOCAL_VALUE=keep\n"
	const changedSettings = `{"model":{"name":"gemini-3-pro"},"security":{"auth":{"selectedType":"vertex-ai"}}}`
	writeFile(t, target.Path, `{"model":{"name":"gemini-3-flash"},"security":{"auth":{"selectedType":"oauth-personal"}}}`)
	writeFile(t, env, originalEnv)
	config, err := edit.Target(target.Path)
	if err != nil {
		t.Fatal(err)
	}
	edited := false
	err = edit.Filtered(func(path string, body []byte) []byte {
		if path == config && !edited {
			edited = true
			writeFile(t, target.Path, changedSettings)
		}
		return body
	}, func() error { return target.Apply("model", "fake/m1") })
	if !edited || !errors.Is(err, extension.ErrActionChanged) {
		t.Fatalf("Apply = %v, config edit injected = %t", err, edited)
	}
	if readFile(target.Path) != changedSettings || readFile(env) != originalEnv || len(peek("gemini.model", "gemini.auth", "gemini.api_key")) != 0 {
		t.Fatal("failed Apply retained a partial route or lost the user's edit")
	}
	if err := target.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	if err := target.Disconnect(); err != nil {
		t.Fatal(err)
	}
	model, _ := edit.GetJSON(target.Path, "model.name")
	auth, _ := edit.GetJSON(target.Path, "security.auth.selectedType")
	key, _ := edit.GetEnvFile(env, "GEMINI_API_KEY")
	if model != "gemini-3-pro" || auth != "vertex-ai" || key != "own-key" {
		t.Fatal("Disconnect did not restore the settings captured on retry")
	}
}

func TestGeminiDisconnectKeepsRouteAndRestoreValuesOnFailure(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		name := "malformed"
		if concurrent {
			name = "external edit"
		}
		t.Run(name, func(t *testing.T) {
			home, _ := codexHome(t, "", "")
			target := gemini(home)
			env := filepath.Join(target.Dir, ".env")
			writeFile(t, target.Path, `{"model":{"name":"gemini-3-flash"},"security":{"auth":{"selectedType":"oauth-personal"}}}`)
			if err := target.Apply("model", "fake/m1"); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(env, 0o600); err != nil {
				t.Fatal(err)
			}
			routedEnv := readFile(env)
			keys := []string{"gemini.base_url", "gemini.api_key", "gemini.auth", "gemini.model"}
			saved := peek(keys...)
			var err error
			changed := `{"model":[`
			if concurrent {
				changed = `{"model":{"name":"fake/m1"},"manual":true}`
				config, targetErr := edit.Target(target.Path)
				if targetErr != nil {
					t.Fatal(targetErr)
				}
				injected := false
				err = edit.Filtered(func(path string, body []byte) []byte {
					if path == config && !injected {
						injected = true
						writeFile(t, target.Path, changed)
					}
					return body
				}, target.Disconnect)
				if !injected || !errors.Is(err, extension.ErrActionChanged) {
					t.Fatalf("Disconnect = %v, config edit injected = %t", err, injected)
				}
			} else {
				writeFile(t, target.Path, changed)
				err = target.Disconnect()
				if err == nil {
					t.Fatal("Disconnect accepted malformed settings")
				}
			}
			body, readErr := os.ReadFile(env)
			if readErr != nil || string(body) != routedEnv || !reflect.DeepEqual(peek(keys...), saved) {
				t.Fatal("failed Disconnect removed the routed environment or consumed restoration values")
			}
			if info, statErr := os.Stat(env); statErr != nil || info.Mode().Perm() != 0o600 {
				t.Fatal("failed Disconnect changed the key file's permissions")
			}
			if readFile(target.Path) != changed {
				t.Fatal("failed Disconnect overwrote the settings")
			}
		})
	}
}
