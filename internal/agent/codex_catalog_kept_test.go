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
	"github.com/yetone/magpie/internal/provider"
)

func TestCodexSyncKeepsConcurrentCatalogEdit(t *testing.T) {
	for _, scope := range []string{"global", "profile"} {
		t.Run(scope, func(t *testing.T) {
			home, _ := codexHome(t, "", "")
			cx := codex(home)
			if scope == "profile" {
				if err := cx.Field("scope").Set(scope); err != nil {
					t.Fatal(err)
				}
				cx = codex(home)
			}
			if err := cx.Apply("model", "fake/m1"); err != nil {
				t.Fatal(err)
			}
			catalogPath := filepath.Join(cx.Dir, "magpie-models.json")
			writeFile(t, catalogPath, `{"models":[]}`)
			target, err := edit.Target(catalogPath)
			if err != nil {
				t.Fatal(err)
			}
			const user = `{"models":[{"slug":"fake/m1","personal_setting":"concurrent"}]}`
			var edited bool
			err = edit.Filtered(func(path string, body []byte) []byte {
				if path == target && !edited {
					writeFile(t, target, user)
					edited = true
				}
				return body
			}, cx.Sync)
			if !edited || !errors.Is(err, extension.ErrActionChanged) {
				t.Fatalf("sync did not refuse the stale catalog: edited=%v, err=%v", edited, err)
			}
			if got := readFile(catalogPath); got != user {
				t.Fatalf("sync overwrote a concurrent catalog edit: %q", got)
			}
			if err := cx.Sync(); err != nil {
				t.Fatal(err)
			}
			if got := readFile(catalogPath); !strings.Contains(got, `"concurrent"`) ||
				!strings.Contains(got, `"supports_parallel_tool_calls"`) {
				t.Fatal("the next sync did not keep both the user setting and the generated catalog")
			}
		})
	}
}

func TestCodexSyncRejectsAliasedCatalog(t *testing.T) {
	for _, scope := range []string{"global", "profile"} {
		for _, link := range []struct {
			name string
			make func(string, string) error
		}{{"symlink", os.Symlink}, {"hardlink", os.Link}} {
			t.Run(scope+"/"+link.name, func(t *testing.T) {
				home, _ := codexHome(t, "", "")
				cx := codex(home)
				if scope == "profile" {
					if err := cx.Field("scope").Set(scope); err != nil {
						t.Fatal(err)
					}
					cx = codex(home)
				}
				if err := cx.Apply("model", "fake/m1"); err != nil {
					t.Fatal(err)
				}
				config := readFile(cx.Path)
				catalog := filepath.Join(cx.Dir, "magpie-models.json")
				if err := os.Remove(catalog); err != nil {
					t.Fatal(err)
				}
				if err := link.make(cx.Path, catalog); err != nil {
					t.Skip(err)
				}
				if err := cx.Sync(); err == nil || !strings.Contains(err.Error(), "name the same file") {
					t.Fatalf("sync accepted a catalog alias: %v", err)
				}
				if got := readFile(cx.Path); got != config || readFile(catalog) != config {
					t.Fatalf("sync changed the aliased config: %q", got)
				}
				settings, settingsErr := os.Stat(cx.Path)
				catalogInfo, catalogErr := os.Stat(catalog)
				if settingsErr != nil || catalogErr != nil || !os.SameFile(settings, catalogInfo) {
					t.Fatalf("sync broke the alias: config error=%v catalog error=%v", settingsErr, catalogErr)
				}
			})
		}
	}
}

// What the user adds by hand to the model list magpie writes for Codex
// stays through the next model switch, sync and stepping out (#1450:
// supports_reasoning_summaries, put in for Codex to show a model's
// thinking, was gone after the next change). A key magpie writes, or took
// out on purpose (multi_agent_version with the V1 setting off), is
// magpie's.
func TestCodexCatalogKeepsWhatTheUserAdded(t *testing.T) {
	home, _ := codexHome(t, "", "")
	list := filepath.Join(home, ".codex", "magpie-models.json")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	// the file as magpie wrote it, edited by hand: two keys on fake/m1's
	// entry, and a version magpie no longer writes
	var cat map[string]any
	if err := json.Unmarshal([]byte(readFile(list)), &cat); err != nil {
		t.Fatal(err)
	}
	e := cat["models"].([]any)[0].(map[string]any)
	if e["slug"] != "fake/m1" {
		t.Fatalf("first entry: %v", e["slug"])
	}
	e["supports_reasoning_summaries"] = true
	e["default_reasoning_summary"] = "detailed"
	e["multi_agent_version"] = "v1"
	b, _ := json.MarshalIndent(cat, "", "  ")
	writeFile(t, list, string(b))

	has := func(when string) {
		t.Helper()
		var got struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal([]byte(readFile(list)), &got); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		var m1 map[string]any
		for _, m := range got.Models {
			if m["slug"] == "fake/m1" {
				m1 = m
			}
		}
		if m1 == nil || m1["supports_reasoning_summaries"] != true || m1["default_reasoning_summary"] != "detailed" {
			t.Errorf("%s: the user's keys are gone: %v %v", when, m1["supports_reasoning_summaries"], m1["default_reasoning_summary"])
		}
		// magpie writes v2 for a model it offers Ultra (pingqipai3 on X),
		// never the user's v1 back
		if v, ok := m1["multi_agent_version"]; ok && v != "v2" {
			t.Errorf("%s: a version magpie no longer writes stayed: %v", when, v)
		}
		if m1["base_instructions"] == nil || m1["supports_parallel_tool_calls"] != true {
			t.Errorf("%s: magpie's own keys are gone", when)
		}
	}

	// a provider added: the sync writes the list again
	if err := provider.Save(provider.Provider{ID: "added", Name: "Added", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"m2"}}); err != nil {
		t.Fatal(err)
	}
	SyncCatalog()
	if !strings.Contains(readFile(list), `"added/m2"`) {
		t.Fatal("sync didn't write the new model")
	}
	has("after a sync")

	if err := cx.Fields[0].Set("added/m2"); err != nil {
		t.Fatal(err)
	}
	has("after a model switch")

	// Codex's own default: magpie steps out, and the list the user added
	// to stays for when magpie is back
	if err := cx.Fields[0].Set(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(list); err != nil {
		t.Fatalf("the list the user added to was removed: %v", err)
	}
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	has("after stepping out and back")
}

// A list holding nothing of the user's still goes as magpie steps out.
func TestCodexCatalogRemovedWhenNothingAdded(t *testing.T) {
	home, _ := codexHome(t, "", "")
	list := filepath.Join(home, ".codex", "magpie-models.json")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(list); err != nil {
		t.Fatal(err)
	}
	if err := cx.Fields[0].Set(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(list); err == nil {
		t.Error("magpie's list stayed after it stepped out")
	}
}
