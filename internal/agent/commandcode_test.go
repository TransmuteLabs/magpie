package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
)

func TestCommandCodeRejectsConcurrentSettingsChanges(t *testing.T) {
	for _, action := range []string{"model", "sync", "effort"} {
		t.Run(action, func(t *testing.T) {
			home, _ := codexHome(t, "", "")
			target := commandCode(home)
			providers := filepath.Join(target.Dir, "providers.json")
			writeFile(t, target.Path, `{"model":"native/gpt","modelProvider":"native"}`)
			writeFile(t, providers, `{"provider":{"native":{"name":"Mine"},"magpie":{"models":{"old":{}}}}}`)
			path := target.Path
			change := func() error { return target.Apply("model", "magpie/fake/m1") }
			switch action {
			case "sync":
				path, change = providers, target.Sync
			case "effort":
				change = func() error { return target.Apply("effort", "high") }
			}
			want := `{"model":"native/changed","manual":"keep"}`
			other := providers
			if action == "sync" {
				want = `{"provider":{"native":{"name":"Edited outside Magpie"}}}`
				other = target.Path
			}
			before := readFile(other)
			resolved, err := edit.Target(path)
			if err != nil {
				t.Fatal(err)
			}
			injected := false
			err = edit.Filtered(func(writing string, body []byte) []byte {
				if writing == resolved && !injected {
					injected = true
					writeFile(t, path, want)
				}
				return body
			}, change)
			if !injected || !errors.Is(err, extension.ErrActionChanged) {
				t.Errorf("%s: %v, external change injected: %t", action, err, injected)
			}
			if got := readFile(path); got != want {
				t.Errorf("overwrote the external change: %s", got)
			}
			if got := readFile(other); got != before {
				t.Errorf("left a partial settings change: %s", got)
			}
		})
	}
}

func TestCommandCode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Chat: "https://api.deepseek.com/v1", Key: "k", Models: []string{"pro", "flash"}}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".commandcode")
	settingsPath, providersPath := filepath.Join(dir, "settings.json"), filepath.Join(dir, "providers.json")
	read := func(p string) map[string]any {
		var m map[string]any
		b, _ := os.ReadFile(p)
		json.Unmarshal(b, &m)
		return m
	}
	os.MkdirAll(dir, 0o755)
	os.WriteFile(settingsPath, []byte(`{"theme":"dark","model":"deepseek/deepseek-v4-flash"}`), 0o644)
	os.WriteFile(providersPath, []byte(`{"provider":{"mine":{"name":"Mine","api":"openai-completions","baseURL":"https://x","apiKey":"$X","models":{"a":{}}}}}`), 0o644)

	f := commandCode(home).Field("model")
	if f.Get() != "deepseek/deepseek-v4-flash" {
		t.Fatalf("get: %q", f.Get())
	}
	if err := f.Set("magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	s, p := read(settingsPath), read(providersPath)
	if s["model"] != "magpie/deepseek/pro" || s["modelProvider"] != "magpie" || s["theme"] != "dark" {
		t.Fatalf("settings: %v", s)
	}
	ps := p["provider"].(map[string]any)
	m, ok := ps["magpie"].(map[string]any)
	if !ok || ps["mine"] == nil || m["apiKey"] != false || m["api"] != "openai-completions" {
		t.Fatalf("providers: %v", p)
	}
	if ms := m["models"].(map[string]any); ms["deepseek/pro"] == nil || ms["deepseek/flash"] == nil {
		t.Fatalf("models: %v", ms)
	}

	// its own model: magpie steps out
	if err := f.Set("moonshotai/kimi-k3"); err != nil {
		t.Fatal(err)
	}
	s, p = read(settingsPath), read(providersPath)
	if s["model"] != "moonshotai/kimi-k3" || s["modelProvider"] != nil || p["provider"].(map[string]any)["magpie"] != nil || p["provider"].(map[string]any)["mine"] == nil {
		t.Fatalf("own: %v %v", s, p)
	}

	// through magpie, then reset
	f.Set("magpie/deepseek/flash")
	if err := f.Set(""); err != nil {
		t.Fatal(err)
	}
	s, p = read(settingsPath), read(providersPath)
	if s["model"] != nil || s["modelProvider"] != nil || s["theme"] != "dark" || p["provider"].(map[string]any)["magpie"] != nil {
		t.Fatalf("reset: %v %v", s, p)
	}
}

// Command Code reads a model's window and reply limit from providers.json
// alone — its catalog lookup keys on the bare vendor id and never matches
// magpie's "magpie/provider/model", and it cannot interrogate the gateway's
// /v1/models — so a model with no contextWindow there is served to it as
// Command Code's own 200K default, a 1M-window one included (#5567). The
// limits are those every other writer in this package hands out (m.Context,
// maxTokens), and a model that declares no output carries none.
func TestCommandCodeModelLimits(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Chat: "https://api.deepseek.com/v1", Key: "k", Models: []string{"pro", "flash"}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("deepseek", "https://api.deepseek.com/v1", []catalog.Model{
		{ID: "pro", Context: 1048576, Output: 393216, Reasoning: true},
		{ID: "flash", Context: 1000000},
	}); err != nil {
		t.Fatal(err)
	}
	f := commandCode(home).Field("model")
	if err := f.Set("magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".commandcode", "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var written struct {
		Provider struct {
			Magpie struct {
				Models map[string]map[string]any `json:"models"`
			} `json:"magpie"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(b, &written); err != nil {
		t.Fatal(err)
	}
	model := func(id string) map[string]any {
		t.Helper()
		m, ok := written.Provider.Magpie.Models[id]
		if !ok {
			t.Fatalf("%s not in %v", id, written.Provider.Magpie.Models)
		}
		return m
	}
	if got := model("deepseek/pro")["contextWindow"]; got != float64(1048576) {
		t.Fatalf("pro contextWindow %v, want 1048576", got)
	}
	if got := model("deepseek/pro")["maxOutput"]; got != float64(393216) {
		t.Fatalf("pro maxOutput %v, want 393216", got)
	}
	if got := model("deepseek/flash")["contextWindow"]; got != float64(1000000) {
		t.Fatalf("flash contextWindow %v, want 1000000", got)
	}
	if got, ok := model("deepseek/flash")["maxOutput"]; ok {
		t.Fatalf("flash carries maxOutput %v with no output declared", got)
	}
	if model("deepseek/pro")["reasoning"] != true {
		t.Fatalf("pro reasoning %v, want true", model("deepseek/pro")["reasoning"])
	}
}
