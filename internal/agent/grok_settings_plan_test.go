package agent

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
)

func TestGrokLikeSettingsPreserveConcurrentEdits(t *testing.T) {
	const selected = "magpie/local/near"
	const original = "[models]\ndefault = \"" + selected + "\"\ndefault_reasoning_effort = \"medium\"\n\n" +
		"[features]\ncampaigns = true\n\n[model.\"" + selected + "\"]\nmodel = \"local/near\"\ncontext_window = 12345\n"
	const revised = original + "\n[ui]\ntheme = \"user edit\"\n"
	for _, kind := range []grokKind{grokBuild, kigiKind} {
		for _, action := range []struct {
			name, field, value string
		}{
			{name: "gateway", field: "model", value: selected},
			{name: "native", field: "model", value: "native-model"},
			{name: "reset", field: "model"},
			{name: "effort", field: "effort", value: "high"},
			{name: "clear-effort", field: "effort"},
			{name: "sync"},
		} {
			t.Run(kind.id+"/"+action.name, func(t *testing.T) {
				home := syncHome(t)
				t.Setenv(kind.env, "")
				if err := provider.Save(provider.Provider{
					ID: "local", Name: "Local", Chat: "http://127.0.0.1:9/v1",
					Key: "test", Models: []string{"near"}, Contexts: map[string]int{"near": 50000},
				}); err != nil {
					t.Fatal(err)
				}
				agent := grokLikeIn(kind, here(home))
				path := agent.Path
				writeFile(t, path, original)
				windows, percents := kind.id+":"+path+":windows", kind.id+":"+path+":percents"
				const savedWindows, savedPercents = `{"local/near":12345}`, `{"local/near":73}`
				stash(map[string]string{windows: savedWindows, percents: savedPercents})
				target, err := edit.Target(path)
				if err != nil {
					t.Fatal(err)
				}
				var changed bool
				err = edit.Filtered(func(path string, body []byte) []byte {
					if path == target && !changed {
						writeFile(t, target, revised)
						changed = true
					}
					return body
				}, func() error {
					if action.field == "" {
						return agent.Sync()
					}
					return agent.Apply(action.field, action.value)
				})
				if !changed || !errors.Is(err, extension.ErrActionChanged) {
					t.Errorf("concurrent change accepted: edited=%t, err=%v", changed, err)
				}
				if got := readFile(path); got != revised {
					t.Errorf("concurrent change was overwritten: %q", got)
				}
				if got := peek(windows, percents); got[windows] != savedWindows || got[percents] != savedPercents {
					t.Errorf("rejected settings change altered recovery: %v", got)
				}
				if agent.Path != filepath.Join(home, kind.dir, "config.toml") {
					t.Errorf("agent used unexpected settings path %s", agent.Path)
				}
			})
		}
	}
}

func TestGrokLikeSettingsPublishModelAndEffortTogether(t *testing.T) {
	const url = "http://127.0.0.1:9/v1"
	const selected = "magpie/local/near"
	const original = "\ufeff# user settings\r\n[models]\r\ndefault = 'native' # choice\r\n" +
		"default_reasoning_effort = 'xhigh' # reasoning\r\n\r\n[ui]\r\ntheme = \"dark\"\r\n"
	for _, kind := range []grokKind{grokBuild, kigiKind} {
		t.Run(kind.id, func(t *testing.T) {
			home := syncHome(t)
			t.Setenv(kind.env, "")
			if err := provider.Save(provider.Provider{ID: "local", Name: "Local", Chat: url, Key: "test", Models: []string{"near"}}); err != nil {
				t.Fatal(err)
			}
			if err := catalog.SaveLive("local", url, []catalog.Model{{ID: "near", Context: 50000, Efforts: []string{"high", "low"}}}); err != nil {
				t.Fatal(err)
			}
			agent := grokLikeIn(kind, here(home))
			writeFile(t, agent.Path, original)
			target, err := edit.Target(agent.Path)
			if err != nil {
				t.Fatal(err)
			}
			windows, percents := kind.id+":"+agent.Path+":windows", kind.id+":"+agent.Path+":percents"
			var writes int
			err = edit.Filtered(func(path string, body []byte) []byte {
				if path == target {
					writes++
					if got := peek(windows, percents); len(got) != 0 {
						t.Errorf("recovery was visible before settings publication: %v", got)
					}
				}
				return body
			}, func() error { return agent.Apply("model", selected) })
			if err != nil || writes != 1 {
				t.Fatalf("settings publication: writes=%d, err=%v", writes, err)
			}
			raw := readFile(agent.Path)
			if !strings.HasPrefix(raw, "\ufeff# user settings\r\n") || strings.Contains(strings.ReplaceAll(raw, "\r\n", ""), "\n") ||
				!strings.Contains(raw, `default = "`+selected+`" # choice`) ||
				!strings.Contains(raw, `default_reasoning_effort = "high" # reasoning`) ||
				!strings.Contains(raw, "[ui]\r\ntheme = \"dark\"\r\n") ||
				!strings.Contains(raw, "campaigns = false\r\n") ||
				!strings.Contains(raw, "context_window = 50000\r\n") ||
				!strings.Contains(raw, "reasoning_efforts = [\"low\", \"high\"]\r\n") {
				t.Errorf("settings did not preserve layout, encoding and fit: %q", raw)
			}
			got := peek(windows, percents)
			var recorded map[string]int
			if err := json.Unmarshal([]byte(got[windows]), &recorded); err != nil || recorded["local/near"] != 50000 || got[percents] == "" {
				t.Errorf("published settings left unexpected recovery: %v", got)
			}
			if err := agent.Apply("model", ""); err != nil {
				t.Fatal(err)
			}
			if got := peek(windows, percents); len(got) != 0 {
				t.Errorf("reset kept obsolete recovery: %v", got)
			}
		})
	}
}
