package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
)

func TestCodexSyncRefusesCatalogAliasedToConfig(t *testing.T) {
	for _, scope := range []string{"global", "profile", "profile-global-config"} {
		for _, link := range []struct {
			name string
			make func(string, string) error
		}{
			{name: "hard-link", make: os.Link},
			{name: "symlink", make: os.Symlink},
		} {
			t.Run(scope+"/"+link.name, func(t *testing.T) {
				home, _ := codexHome(t, "", "# the user's global config\n")
				cx := codex(home)
				if scope != "global" {
					if err := cx.Field("scope").Set("profile"); err != nil {
						t.Fatal(err)
					}
					cx = codex(home)
				}
				if err := cx.Apply("model", "fake/m1"); err != nil {
					t.Fatal(err)
				}
				config := cx.Path
				if scope == "profile-global-config" {
					config = filepath.Join(cx.Dir, "config.toml")
				}
				catalogPath := filepath.Join(cx.Dir, "magpie-models.json")
				if err := os.Remove(catalogPath); err != nil {
					t.Fatal(err)
				}
				if err := link.make(config, catalogPath); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("link unavailable: %v", err)
					}
					t.Fatal(err)
				}
				if err := os.Chmod(config, 0o600); err != nil {
					t.Fatal(err)
				}
				want := readFile(config)
				before, err := os.Stat(config)
				if err != nil {
					t.Fatal(err)
				}
				err = cx.Sync()
				after, statErr := os.Stat(catalogPath)
				if !errors.Is(err, extension.ErrActionChanged) || !strings.Contains(err.Error(), "aliases") ||
					readFile(config) != want || statErr != nil ||
					!os.SameFile(before, after) || before.Mode() != after.Mode() {
					t.Fatalf("sync did not preserve an aliased config: err=%v, stat=%v, config changed=%v", err, statErr, readFile(config) != want)
				}
			})
		}
	}
}

func TestCodexSyncRefusesLateCatalogAlias(t *testing.T) {
	home, _ := codexHome(t, "", "")
	cx := codex(home)
	if err := cx.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(cx.Dir, "magpie-models.json")
	writeFile(t, catalogPath, `{"models":[]}`)
	target, err := edit.Target(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	configAfter := readFile(cx.Path) + "\n# external edit while linking\n"
	linked := false
	err = edit.Filtered(func(path string, body []byte) []byte {
		if path == target && !linked {
			if err := os.Remove(catalogPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(cx.Path, catalogPath); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("link unavailable: %v", err)
				}
				t.Fatal(err)
			}
			writeFile(t, cx.Path, configAfter)
			linked = true
		}
		return body
	}, cx.Sync)
	if !linked || !errors.Is(err, extension.ErrActionChanged) || !strings.Contains(err.Error(), "aliases") {
		t.Fatalf("sync did not identify the late alias: linked=%v err=%v", linked, err)
	}
	if readFile(cx.Path) != configAfter || readFile(catalogPath) != configAfter {
		t.Fatal("sync or rollback overwrote the config through the new alias")
	}
}

func TestCodexSyncKeepsConcurrentCatalogEdit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile bool
		link    func(string, string) error
	}{
		{name: "global"},
		{name: "profile", profile: true},
		{name: "global-hard-link", link: os.Link},
		{name: "profile-symlink", profile: true, link: os.Symlink},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := codexHome(t, "", "")
			cx := codex(home)
			if tc.profile {
				if err := cx.Field("scope").Set("profile"); err != nil {
					t.Fatal(err)
				}
				cx = codex(home)
			}
			if err := cx.Apply("model", "fake/m1"); err != nil {
				t.Fatal(err)
			}
			catalogPath := filepath.Join(cx.Dir, "magpie-models.json")
			writeFile(t, catalogPath, `{"models":[]}`)
			aliasTarget := ""
			if tc.link != nil {
				aliasTarget = filepath.Join(cx.Dir, "hand-managed-models.json")
				if err := os.Rename(catalogPath, aliasTarget); err != nil {
					t.Fatal(err)
				}
				if err := tc.link(aliasTarget, catalogPath); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("link unavailable: %v", err)
					}
					t.Fatal(err)
				}
			}
			target, err := edit.Target(catalogPath)
			if err != nil {
				t.Fatal(err)
			}
			const user = `{"models":[{"slug":"fake/m1","personal_setting":"concurrent"}]}`
			var edited bool
			err = edit.Filtered(func(path string, body []byte) []byte {
				if path == target && !edited {
					writeFile(t, target, user)
					if err := os.Chmod(target, 0o600); err != nil {
						t.Fatal(err)
					}
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
			stamp, err := os.Stat(catalogPath)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && stamp.Mode().Perm() != 0o600 {
				t.Fatalf("changed catalog mode = %v", stamp.Mode().Perm())
			}
			if aliasTarget != "" {
				linked, err := os.Stat(aliasTarget)
				if err != nil || !os.SameFile(stamp, linked) {
					t.Fatalf("sync split the catalog from its alias: %v", err)
				}
			}
			if err := cx.Sync(); err != nil {
				t.Fatal(err)
			}
			same, err := os.Stat(catalogPath)
			if err != nil || !os.SameFile(stamp, same) || !stamp.ModTime().Equal(same.ModTime()) {
				t.Fatalf("unchanged sync rewrote the catalog: %v", err)
			}
		})
	}
}

func TestCodexEffortCatalogConflictRollsBackOnlyItsOwnConfig(t *testing.T) {
	for _, scope := range []string{"global", "profile"} {
		for _, scenario := range []struct {
			name       string
			editConfig bool
		}{
			{name: "catalog-only"},
			{name: "catalog-and-config", editConfig: true},
		} {
			t.Run(scope+"/"+scenario.name, func(t *testing.T) {
				home, _ := codexHome(t, "", "")
				cx := codex(home)
				if scope == "profile" {
					if err := cx.Field("scope").Set("profile"); err != nil {
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
				const catalogEdit = `{"models":[{"slug":"external"}]}`
				wantConfig := readFile(cx.Path)
				edited := false
				err = edit.Filtered(func(path string, body []byte) []byte {
					if path == target && !edited {
						edited = true
						withEffort := readFile(cx.Path)
						if !strings.Contains(withEffort, `model_reasoning_effort = "medium"`) {
							t.Fatal("effort was not written before catalog publication")
						}
						if scenario.editConfig {
							wantConfig = withEffort + "\n# user edited config during catalog publication\n"
							writeFile(t, cx.Path, wantConfig)
						}
						writeFile(t, path, catalogEdit)
					}
					return body
				}, func() error { return cx.Apply("effort", "medium") })
				if !edited || !errors.Is(err, extension.ErrActionChanged) {
					t.Fatalf("apply did not reject the stale catalog: edited=%t err=%v", edited, err)
				}
				if readFile(cx.Path) != wantConfig || readFile(catalogPath) != catalogEdit {
					t.Fatal("apply rollback failed to preserve the concurrent edit or restore its own config change")
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
