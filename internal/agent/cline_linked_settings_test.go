package agent

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
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

func TestClineModelChangesKeepSettingsAndStashOnConflict(t *testing.T) {
	for _, action := range []string{"connect", "pick", "native", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			home := syncHome(t)
			t.Setenv("CLINE_DIR", "")
			t.Setenv("CLINE_DATA_DIR", "")
			a := cline(home)
			if action == "pick" {
				if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"glm-4.6", "llama-4"}}); err != nil {
					t.Fatal(err)
				}
			}
			data := filepath.Dir(filepath.Dir(a.Path))
			models := filepath.Join(data, "settings", "models.json")
			state := filepath.Join(data, "globalState.json")
			secrets := filepath.Join(data, "secrets.json")
			native := map[string]string{
				a.Path:  `{"version":1,"lastUsedProvider":"anthropic","providers":{"anthropic":{"settings":{"apiKey":"native-key","model":"native-model","reasoning":{"enabled":false}}}}}`,
				models:  `{"version":1,"providers":{"openai-compatible":{"provider":{"baseUrl":"https://own.invalid/v1"},"models":{"own":{"id":"own"}}}}}`,
				state:   `{"actModeApiProvider":"anthropic","openAiBaseUrl":"https://own.invalid/v1","telemetrySetting":"disabled"}`,
				secrets: `{"openAiApiKey":"own-key","anthropicApiKey":"native-key"}`,
			}
			for path, body := range native {
				writeFile(t, path, body)
			}
			change := func() error { return a.Apply("model", "magpie/relay/glm-4.6") }
			if action == "connect" {
				change = a.Connect
			} else {
				if err := change(); err != nil {
					t.Fatal(err)
				}
				switch action {
				case "pick":
					change = func() error { return a.Apply("model", "magpie/relay/llama-4") }
				case "native":
					change = func() error { return a.Apply("model", "native-new") }
				case "disconnect":
					change = a.Disconnect
				}
			}
			before := map[string]string{}
			for path := range native {
				before[path] = readFile(path)
			}
			prefix := "cline:" + a.Path + ":"
			stashKeys := []string{prefix + "entry", prefix + "lastUsedProvider", prefix + "models",
				prefix + "state", prefix + "secret", prefix + "stated"}
			stashed := peek(stashKeys...)
			user := strings.Replace(before[a.Path], "native-key", "new-native-key", 1)
			if user == before[a.Path] {
				t.Fatal("native key was absent before the conflict")
			}
			target, err := edit.Target(a.Path)
			if err != nil {
				t.Fatal(err)
			}
			modelTarget, err := edit.Target(models)
			if err != nil {
				t.Fatal(err)
			}
			edited := false
			filteredRestore := false
			err = edit.Filtered(func(path string, body []byte) []byte {
				if path == target && !edited {
					edited = true
					writeFile(t, a.Path, user)
				}
				if path == modelTarget && edited {
					filteredRestore = true
					return []byte(`{}`)
				}
				return body
			}, change)
			if !edited || !errors.Is(err, extension.ErrActionChanged) {
				t.Errorf("model change accepted a modified provider file: edited=%t, error=%v", edited, err)
			}
			if filteredRestore {
				t.Error("rollback ran the restored catalog through the publication filter")
			}
			for path, want := range before {
				if path == a.Path {
					want = user
				}
				if readFile(path) != want {
					t.Errorf("rejected model change failed to preserve %s", filepath.Base(path))
				}
			}
			if !maps.Equal(peek(stashKeys...), stashed) {
				t.Error("rejected model change changed the saved Cline originals")
			}
			if t.Failed() {
				return
			}
			if err := change(); err != nil {
				t.Fatal("retry:", err)
			}
			if key, _ := edit.GetJSON(a.Path, "providers.anthropic.settings.apiKey"); key != "new-native-key" {
				t.Error("retry overwrote the user's updated key")
			}
			switch action {
			case "connect", "pick":
				want := "magpie/relay/glm-4.6"
				if action == "pick" {
					want = "magpie/relay/llama-4"
				}
				if got := a.Field("model").Get(); got != want {
					t.Errorf("after retry model = %q, want %q", got, want)
				}
			case "native":
				if got := a.Field("model").Get(); got != "native-new" {
					t.Errorf("after retry model = %q, want native-new", got)
				}
			case "disconnect":
				if got := a.Field("model").Get(); got != "native-model" {
					t.Errorf("after retry model = %q, want native-model", got)
				}
			}
			if action == "native" || action == "disconnect" {
				for path, field := range map[string]string{models: "providers.openai-compatible.provider.baseUrl", state: "openAiBaseUrl", secrets: "openAiApiKey"} {
					want := "https://own.invalid/v1"
					if path == secrets {
						want = "own-key"
					}
					if value, _ := edit.GetJSON(path, field); value != want {
						t.Errorf("retry did not restore %s", filepath.Base(path))
					}
				}
				if len(peek(stashKeys...)) != 0 {
					t.Error("successful restoration kept the saved Cline originals")
				}
			}
		})
	}
}

func TestClineConnectCreatesPrivateSettings(t *testing.T) {
	t.Run("home", func(t *testing.T) { testClineConnectCreatesPrivateSettings(t, false) })
	t.Run("symlinked-data", func(t *testing.T) { testClineConnectCreatesPrivateSettings(t, true) })
}

func testClineConnectCreatesPrivateSettings(t *testing.T, symlinked bool) {
	t.Helper()
	home := syncHome(t)
	t.Setenv("CLINE_DIR", "")
	t.Setenv("CLINE_DATA_DIR", "")
	a := cline(home)
	data := filepath.Dir(filepath.Dir(a.Path))
	state := filepath.Join(data, "globalState.json")
	secrets := filepath.Join(data, "secrets.json")
	writeFile(t, state, `{"telemetrySetting":"disabled"}`)
	if symlinked {
		if runtime.GOOS == "windows" {
			t.Skip("directory symlinks need Windows privileges")
		}
		alias := filepath.Join(home, "cline-data")
		if err := os.Symlink(data, alias); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CLINE_DATA_DIR", alias)
		a = cline(home)
		other := filepath.Join(home, "other-cline-data")
		otherState := filepath.Join(other, "globalState.json")
		writeFile(t, otherState, `{"openAiBaseUrl":"https://other.invalid/v1"}`)
		target, err := filepath.EvalSymlinks(data)
		if err != nil {
			t.Fatal(err)
		}
		retarget := func(next string) {
			t.Helper()
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(next, alias); err != nil {
				t.Fatal(err)
			}
		}
		changed := false
		err = edit.Filtered(func(path string, body []byte) []byte {
			if path == filepath.Join(target, "settings", "models.json") && !changed {
				changed = true
				retarget(other)
			}
			return body
		}, a.Connect)
		if !changed || !errors.Is(err, extension.ErrActionChanged) {
			t.Fatalf("connect did not reject a retargeted directory: changed=%t, err=%v", changed, err)
		}
		for _, root := range []string{data, other} {
			if _, err := os.Stat(filepath.Join(root, "settings", "models.json")); !os.IsNotExist(err) {
				t.Fatalf("rejected connect wrote to %s: %v", root, err)
			}
		}
		if got := readFile(otherState); got != `{"openAiBaseUrl":"https://other.invalid/v1"}` {
			t.Fatalf("rejected connect modified the new target: %s", got)
		}
		retarget(data)
	}
	dir := filepath.Dir(a.Path)
	if err := a.Connect(); err != nil {
		t.Fatal(err)
	}
	if key, _ := edit.GetJSON(secrets, "openAiApiKey"); key != gateway.Token {
		t.Fatal("first connection didn't provision the extension's gateway key")
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{
			dir: 0o700, state: 0o644, a.Path: 0o600, secrets: 0o600, filepath.Join(dir, "models.json"): 0o600,
		} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode := info.Mode().Perm(); mode != want {
				t.Errorf("%s: got mode %o, want %o", filepath.Base(path), mode, want)
			}
		}
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if _, exists := edit.GetJSON(secrets, "openAiApiKey"); exists {
		t.Error("disconnect left the gateway key in the extension's secrets")
	}
	if value, _ := edit.GetJSON(state, "telemetrySetting"); value != "disabled" {
		t.Error("disconnect changed the user's telemetry setting")
	}
}

func TestClineConnectDoesNotRecreateMovedDataDirectoryThroughAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory symlinks need Windows privileges")
	}
	home := syncHome(t)
	t.Setenv("CLINE_DIR", "")
	base := filepath.Join(home, "cline-target")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "cline-alias")
	if err := os.Symlink(base, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLINE_DATA_DIR", filepath.Join(alias, "moved", "data"))
	var gone *appdir.GoneError
	if err := cline(home).Connect(); !errors.As(err, &gone) {
		t.Fatalf("connect accepted a removed data directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "moved")); !os.IsNotExist(err) {
		t.Fatalf("connect made the removed directory again: %v", err)
	}
}
