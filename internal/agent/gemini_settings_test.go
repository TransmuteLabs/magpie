package agent

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

const geminiTestSettings = `{"model":{"name":"gemini-3-flash","keep":true},"security":{"auth":{"selectedType":"oauth-personal"}}}`
const geminiTestEnv = "GEMINI_API_KEY=own-key\nLOCAL_VALUE=keep\n"

func geminiSettingsFixture(t *testing.T) (*Agent, string) {
	t.Helper()
	home, _ := codexHome(t, "", "")
	target := gemini(home)
	env := filepath.Join(target.Dir, ".env")
	writeFile(t, target.Path, geminiTestSettings)
	writeFile(t, env, geminiTestEnv)
	return target, env
}

func geminiTestSaved() map[string]string {
	return peek("gemini.base_url", "gemini.api_key", "gemini.auth", "gemini.model")
}

func TestGeminiRouteKeepsEnvOnMalformedSettings(t *testing.T) {
	target, env := geminiSettingsFixture(t)
	const malformed = `{"security":[`
	writeFile(t, target.Path, malformed)
	stash(map[string]string{"gemini.api_key": "saved-key"})
	saved := geminiTestSaved()
	if err := target.Apply("model", "fake/m1"); err == nil {
		t.Fatal("route accepted malformed settings")
	}
	if readFile(env) != geminiTestEnv || readFile(target.Path) != malformed || !reflect.DeepEqual(geminiTestSaved(), saved) {
		t.Fatal("failed route changed files or restoration values")
	}
}

func TestGeminiCustomWithoutEndpointKeepsActiveRoute(t *testing.T) {
	target, env := geminiSettingsFixture(t)
	if err := target.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	saved, routedEnv, routedConfig := geminiTestSaved(), readFile(env), readFile(target.Path)
	if err := target.Apply("provider", "custom"); err == nil || !strings.Contains(err.Error(), "GOOGLE_GEMINI_BASE_URL") {
		t.Fatalf("custom choice returned %v", err)
	}
	if readFile(env) != routedEnv || readFile(target.Path) != routedConfig || !reflect.DeepEqual(geminiTestSaved(), saved) {
		t.Fatal("refused custom choice changed the active route or consumed saved settings")
	}
}

func TestGeminiSwitchKeepsActiveRouteOnSettingsConflict(t *testing.T) {
	for _, choice := range []struct {
		name, field, value, match string
	}{
		{"native model", "model", "gemini-3-pro", "gemini-3-pro"},
		{"native auth", "provider", "vertex", "vertex-ai"},
		{"model reset", "model", "", ""},
		{"disconnect", "", "", ""},
	} {
		t.Run(choice.name, func(t *testing.T) {
			target, env := geminiSettingsFixture(t)
			if err := target.Apply("model", "fake/m1"); err != nil {
				t.Fatal(err)
			}
			saved, routedEnv := geminiTestSaved(), readFile(env)
			config, err := edit.Target(target.Path)
			if err != nil {
				t.Fatal(err)
			}
			const newer = `{"model":{"name":"fake/m1"},"manual":"keep"}`
			injected := false
			err = edit.Filtered(func(path string, body []byte) []byte {
				if path == config && !injected && strings.Contains(string(body), choice.match) {
					injected = true
					writeFile(t, target.Path, newer)
				}
				return body
			}, func() error {
				if choice.field == "" {
					return target.Disconnect()
				}
				return target.Apply(choice.field, choice.value)
			})
			if !injected || !errors.Is(err, extension.ErrActionChanged) {
				t.Errorf("choice returned %v, user edit injected = %t", err, injected)
			}
			if readFile(env) != routedEnv || readFile(target.Path) != newer || !reflect.DeepEqual(geminiTestSaved(), saved) {
				t.Fatal("failed choice changed the active route, overwrote the user edit, or consumed saved settings")
			}
		})
	}
}

func TestGeminiRouteKeepsNewEnvAndRestoresSettings(t *testing.T) {
	target, env := geminiSettingsFixture(t)
	if err := os.Chmod(env, 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(env)
	if err != nil {
		t.Fatal(err)
	}
	envTarget, err := edit.Target(env)
	if err != nil {
		t.Fatal(err)
	}
	const newer = "GEMINI_API_KEY=new-key\nLOCAL_VALUE=user-edited\n"
	injected, published, keptStash := false, false, false
	err = edit.Filtered(func(path string, body []byte) []byte {
		if path == envTarget && !injected {
			injected = true
			published = strings.Contains(readFile(target.Path), "fake/m1")
			keptStash = len(geminiTestSaved()) == 0
			writeFile(t, env, newer)
		}
		return body
	}, func() error { return target.Apply("model", "fake/m1") })
	if !injected || !errors.Is(err, extension.ErrActionChanged) {
		t.Errorf("route returned %v, user edit injected = %t", err, injected)
	}
	if !published || !keptStash {
		t.Errorf("settings published before env = %t, stash left until success = %t", published, keptStash)
	}
	if readFile(env) != newer || readFile(target.Path) != geminiTestSettings || len(geminiTestSaved()) != 0 {
		t.Fatal("failed route overwrote the env edit or left a partial route")
	}
	if info, err := os.Stat(env); err != nil || info.Mode() != original.Mode() {
		t.Fatalf("failed route changed the key file's mode: %v, %v", info, err)
	}
	if err := target.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	if err := target.Disconnect(); err != nil {
		t.Fatal(err)
	}
	key, _ := edit.GetEnvFile(env, "GEMINI_API_KEY")
	if key != "new-key" || !strings.Contains(readFile(env), "LOCAL_VALUE=user-edited") {
		t.Fatal("disconnect did not restore the configuration captured on retry")
	}
}

func TestGeminiRollbackKeepsNewerSettings(t *testing.T) {
	target, env := geminiSettingsFixture(t)
	envTarget, err := edit.Target(env)
	if err != nil {
		t.Fatal(err)
	}
	const newer = `{"model":{"name":"gemini-3-pro"},"manual":"keep"}`
	injected := false
	err = edit.Filtered(func(path string, body []byte) []byte {
		if path == envTarget && !injected {
			injected = true
			writeFile(t, target.Path, newer)
		}
		return body
	}, func() error { return target.Apply("model", "fake/m1") })
	if !injected || !errors.Is(err, extension.ErrActionChanged) {
		t.Errorf("route returned %v, settings edit injected = %t", err, injected)
	}
	if readFile(env) != geminiTestEnv || readFile(target.Path) != newer || len(geminiTestSaved()) != 0 {
		t.Fatal("rollback overwrote newer settings or left a partial route")
	}
}

func TestGeminiCustomChoiceRestoresSavedURL(t *testing.T) {
	target, env := geminiSettingsFixture(t)
	const endpoint = "https://gemini.example.invalid"
	writeFile(t, env, geminiTestEnv+"GOOGLE_GEMINI_BASE_URL="+endpoint+"\n")
	if err := target.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	if err := target.Apply("provider", "custom"); err != nil {
		t.Fatal(err)
	}
	base, _ := edit.GetEnvFile(env, "GOOGLE_GEMINI_BASE_URL")
	key, _ := edit.GetEnvFile(env, "GEMINI_API_KEY")
	model, _ := edit.GetJSON(target.Path, "model.name")
	if base != endpoint || key != "own-key" || model != "gemini-3-flash" || len(geminiTestSaved()) != 0 {
		t.Fatal("custom choice did not restore the saved URL, key and model")
	}
}
