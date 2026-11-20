package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

// Codex wired by its profile alone (hpnjstar on X): codex -p magpie goes
// through magpie, and config.toml, which plain codex reads, is not touched
// by connecting, switching models or disconnecting.
func TestCodexProfileOnlyLeavesConfigTomlAlone(t *testing.T) {
	const own = "model = \"gpt-5.5\"\nmodel_provider = \"acme\"\nopenai_base_url = \"https://relay.example/v1\"\n\n[model_providers.acme]\nname = \"acme\"\nbase_url = \"https://acme.example/v1\"\n"
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, own)
	dir := filepath.Join(home, ".codex")
	cx := codex(home)
	if err := cx.Field("scope").Set("profile"); err != nil {
		t.Fatal(err)
	}
	cx = codex(home)
	if cx.Path != filepath.Join(dir, "magpie.config.toml") {
		t.Fatalf("path %s", cx.Path)
	}
	if err := cx.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	prof, _ := os.ReadFile(cx.Path)
	for _, want := range []string{`model_provider = "magpie"`, `model = "fake/m1"`, `model_catalog_json = "magpie-models.json"`, "[model_providers.magpie]"} {
		if !strings.Contains(string(prof), want) {
			t.Errorf("profile lacks %q:\n%s", want, prof)
		}
	}
	if read() != own {
		t.Fatalf("config.toml changed:\n%s", read())
	}
	if !cx.Wired() {
		t.Fatalf("not connected:\n%s", prof)
	}
	if d := cx.Drift(); d != nil {
		t.Fatalf("drift %+v", d)
	}
	if err := cx.Apply("effort", "high"); err != nil {
		t.Fatal(err)
	}
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if read() != own {
		t.Fatalf("config.toml changed after effort and sync:\n%s", read())
	}
	// a way around the scope while connected would leave both wirings
	if err := cx.Field("scope").Set(""); err == nil {
		t.Fatal("scope changed while connected")
	}
	if err := cx.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cx.Path); !os.IsNotExist(err) {
		t.Fatalf("profile left: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "magpie-models.json")); !os.IsNotExist(err) {
		t.Fatalf("catalog left: %v", err)
	}
	if read() != own || codex(home).Wired() {
		t.Fatalf("after disconnect:\n%s", read())
	}
}

// A model of Codex's own is not a profile model: plain codex runs those.
func TestCodexProfileOnlyRefusesAnOwnModel(t *testing.T) {
	home, _ := codexHome(t, "", "")
	cx := codex(home)
	if err := cx.Field("scope").Set("profile"); err != nil {
		t.Fatal(err)
	}
	if err := codex(home).Field("model").Set("gpt-5.5"); err == nil {
		t.Fatal("an own model was written to the profile")
	}
}

func TestCodexProfileOnlyKeepsTheUsersLinkedProfile(t *testing.T) {
	home, read := codexHome(t, "", "model = \"gpt-5.5\"\n")
	dir := filepath.Join(home, "codex-profile-home")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", dir)
	const plain = "model = \"gpt-5.5\"\n"
	writeFile(t, filepath.Join(dir, "config.toml"), plain)
	secret, err := access.Update("add-key", access.Change{Name: "codex-profile"})
	if err != nil {
		t.Fatal(err)
	}
	config := "\xef\xbb\xbfmodel_reasoning_effort = \"high\"\r\n\r\n" +
		"[model_providers.magpie]\r\nexperimental_bearer_token = \"" + secret + "\"\r\n\r\n" +
		"[model_providers.magpie.http_headers]\r\n# my proxy\r\n\"x-org\" = \"acme\"\r\n"
	target := filepath.Join(t.TempDir(), "profile.toml")
	writeFile(t, target, config)
	cx := codex(home)
	if err := cx.Field("scope").Set("profile"); err != nil {
		t.Fatal(err)
	}
	cx = codex(home)
	if err := os.Symlink(target, cx.Path); err != nil {
		t.Skip(err)
	}
	catalog := filepath.Join(dir, "magpie-models.json")
	writeFile(t, catalog, `{"models":[{"slug":"fake/m1","personal_setting":"keep-this"}]}`)
	if err := cx.Connect(); err != nil {
		t.Fatal(err)
	}
	if link, err := os.Readlink(cx.Path); err != nil || link != target {
		t.Fatalf("profile link %q: %v", link, err)
	}
	got := readFile(target)
	for label, want := range map[string]string{
		"BOM and effort":  "\xef\xbb\xbfmodel_reasoning_effort = \"high\"\r\n",
		"retained bearer": "experimental_bearer_token = \"" + secret + "\"\r\n",
		"selected model":  "model = \"fake/m1\"\r\n",
		"user header":     "[model_providers.magpie.http_headers]\r\n# my proxy\r\n\"x-org\" = \"acme\"\r\n",
		"actor header":    "\"x-openai-actor-authorization\" = \"magpie\"\r\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("profile lacks %s", label)
		}
	}
	if got := readFile(catalog); !strings.Contains(got, `"keep-this"`) || !strings.Contains(got, `"default_reasoning_level": "high"`) {
		t.Fatal("catalog lost the user's setting or the profile's effort")
	}
	const malformed = `{"models":[{"slug":"fake/m1"`
	writeFile(t, catalog, malformed)
	if err := cx.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(catalog + ".bad-*")
	if err != nil || len(backups) != 1 || readFile(backups[0]) != malformed {
		t.Fatalf("malformed catalog not preserved: found %d backups, %v", len(backups), err)
	}
	if err := edit.SetTOMLKey(cx.Path, "model_providers.magpie", "base_url", "http://127.0.0.1:1/v1"); err != nil {
		t.Fatal(err)
	}
	if cx.Check() == "" {
		t.Fatal("fixture did not give Codex a stale gateway URL")
	}
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if drift := cx.Check(); drift != "" {
		t.Fatal(drift)
	}
	if readFile(filepath.Join(dir, "config.toml")) != plain || read() != plain {
		t.Fatal("plain Codex config changed")
	}
}

func TestCodexProfileOnlyRejectsAnInvalidPlanBeforeWriting(t *testing.T) {
	home, _ := codexHome(t, "", "")
	cx := codex(home)
	if err := cx.Field("scope").Set("profile"); err != nil {
		t.Fatal(err)
	}
	cx = codex(home)
	const profile = "model.custom = \"keep\"\n"
	writeFile(t, cx.Path, profile)
	catalog := filepath.Join(cx.Dir, "magpie-models.json")
	const original = `{"models":[{"slug":"fake/m1","personal_setting":"keep-this"}]}`
	writeFile(t, catalog, original)
	profileTarget, err := edit.Target(cx.Path)
	if err != nil {
		t.Fatal(err)
	}
	catalogTarget, err := edit.Target(catalog)
	if err != nil {
		t.Fatal(err)
	}
	var writes int
	err = edit.Filtered(func(path string, data []byte) []byte {
		if path == profileTarget || path == catalogTarget {
			writes++
		}
		return data
	}, func() error { return cx.Apply("model", "fake/m1") })
	if err == nil || !strings.Contains(err.Error(), "edited TOML is invalid") {
		t.Fatalf("invalid plan returned %v", err)
	}
	if writes != 0 || readFile(cx.Path) != profile || readFile(catalog) != original {
		t.Fatalf("invalid plan reached the writer %d times", writes)
	}
}

func TestCodexProfileOnlyRejectsAliasedCatalog(t *testing.T) {
	home, _ := codexHome(t, "", "")
	cx := codex(home)
	if err := cx.Field("scope").Set("profile"); err != nil {
		t.Fatal(err)
	}
	cx = codex(home)
	const profile = "model_reasoning_effort = \"high\"\n"
	writeFile(t, cx.Path, profile)
	catalog := filepath.Join(cx.Dir, "magpie-models.json")
	if err := os.Symlink(cx.Path, catalog); err != nil {
		t.Skip(err)
	}
	if err := cx.Apply("model", "fake/m1"); err == nil || !strings.Contains(err.Error(), "name the same file") {
		t.Fatalf("profile accepted a catalog at the same physical file: %v", err)
	}
	if got := readFile(cx.Path); got != profile {
		t.Fatalf("profile changed on rejection: %s", got)
	}
	if got, err := os.Readlink(catalog); err != nil || got != cx.Path {
		t.Fatalf("catalog link changed: %q, %v", got, err)
	}
}

func TestCodexProfileOnlyKeepsConcurrentEditDuringRollback(t *testing.T) {
	home, _ := codexHome(t, "", "")
	cx := codex(home)
	if err := cx.Field("scope").Set("profile"); err != nil {
		t.Fatal(err)
	}
	cx = codex(home)
	writeFile(t, cx.Path, "model_reasoning_effort = \"high\"\n")
	catalog := filepath.Join(cx.Dir, "magpie-models.json")
	const prior = `{"models":[{"slug":"fake/m1","personal_setting":"keep-this"}]}`
	const user = "model_reasoning_effort = \"low\"\n# edited by the user\n"
	writeFile(t, catalog, prior)
	catalogTarget, err := edit.Target(catalog)
	if err != nil {
		t.Fatal(err)
	}
	var edited bool
	err = edit.Filtered(func(path string, data []byte) []byte {
		if path == catalogTarget && !edited {
			writeFile(t, cx.Path, user)
			edited = true
		}
		return data
	}, func() error { return cx.Apply("model", "fake/m1") })
	if !edited || !errors.Is(err, extension.ErrActionChanged) {
		t.Fatalf("Apply did not detect the edit: injected=%v, error=%v", edited, err)
	}
	if got := readFile(cx.Path); got != user {
		t.Fatalf("rollback overwrote the user's edit: %q", got)
	}
	if got := readFile(catalog); got != prior {
		t.Fatalf("catalog rollback returned %q, want %q", got, prior)
	}
}

func TestCodexProfileOnlyKeepsBothConcurrentEditsAfterPublication(t *testing.T) {
	home, _ := codexHome(t, "", "")
	cx := codex(home)
	if err := cx.Field("scope").Set("profile"); err != nil {
		t.Fatal(err)
	}
	cx = codex(home)
	writeFile(t, cx.Path, "model_reasoning_effort = \"high\"\n")
	catalog := filepath.Join(cx.Dir, "magpie-models.json")
	writeFile(t, catalog, `{"models":[{"slug":"fake/m1","personal_setting":"prior"}]}`)
	profileTarget, err := edit.Target(cx.Path)
	if err != nil {
		t.Fatal(err)
	}
	const userConfig = "model_reasoning_effort = \"low\"\n# changed during publication\n"
	const userCatalog = `{"models":[{"slug":"fake/m1","personal_setting":"new edit"}]}`
	var edited bool
	err = edit.Filtered(func(path string, data []byte) []byte {
		if path == profileTarget && !edited && strings.Contains(string(data), "model_provider") {
			if !strings.Contains(readFile(catalog), `"default_reasoning_level": "high"`) {
				t.Fatal("catalog was not published before the config write")
			}
			writeFile(t, cx.Path, userConfig)
			writeFile(t, catalog, userCatalog)
			edited = true
		}
		return data
	}, func() error { return cx.Apply("model", "fake/m1") })
	if !edited || !errors.Is(err, extension.ErrActionChanged) {
		t.Fatalf("apply accepted stale settings: edited=%v, err=%v", edited, err)
	}
	if got := readFile(cx.Path); got != userConfig {
		t.Fatalf("the user's profile edit was lost: %q", got)
	}
	if got := readFile(catalog); got != userCatalog {
		t.Fatalf("the user's catalog edit was lost: %q", got)
	}
}

func TestCodexProfileOnlyRollsBackWhenProfilePublicationFails(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs file permission checks")
	}
	home, _ := codexHome(t, "", "")
	cx := codex(home)
	if err := cx.Field("scope").Set("profile"); err != nil {
		t.Fatal(err)
	}
	cx = codex(home)
	const profile = "model_reasoning_effort = \"high\"\n"
	writeFile(t, cx.Path, profile)
	catalog := filepath.Join(cx.Dir, "magpie-models.json")
	target := filepath.Join(t.TempDir(), "models.json")
	const original = `{"models":[{"slug":"fake/m1","personal_setting":"keep-this"}]}`
	writeFile(t, target, original)
	if err := os.Symlink(target, catalog); err != nil {
		t.Skip(err)
	}
	profileTarget, err := edit.Target(cx.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cx.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(cx.Dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	var catalogPublished bool
	err = edit.Filtered(func(path string, data []byte) []byte {
		if path == profileTarget {
			catalogPublished = strings.Contains(readFile(target), `"default_reasoning_level": "high"`)
		}
		return data
	}, func() error { return cx.Apply("model", "fake/m1") })
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("expected a profile write failure, got %v", err)
	}
	if !catalogPublished || readFile(target) != original || readFile(cx.Path) != profile {
		t.Fatalf("catalog published before the error %v, final files were not restored", catalogPublished)
	}
}

func TestCodexEffortKeepsConfigWhenCatalogCannotBeUpdated(t *testing.T) {
	for _, scope := range []string{"global", "profile"} {
		t.Run(scope, func(t *testing.T) {
			home, _ := codexHome(t, "", "model_reasoning_effort = \"high\"\n")
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
			before := readFile(cx.Path)
			catalog := filepath.Join(cx.Dir, "magpie-models.json")
			if err := os.Remove(catalog); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(catalog, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := cx.Field("effort").Set("low"); err == nil {
				t.Fatal("accepted an unwritable catalog")
			}
			if got := readFile(cx.Path); got != before {
				t.Fatalf("the failed effort change left a new configuration:\n%s", got)
			}
		})
	}
}

// The default wiring still writes config.toml, and "applies to" refuses to
// move under it.
func TestCodexScopeWaitsForDisconnect(t *testing.T) {
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, "")
	cx := codex(home)
	if err := cx.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(), "openai_base_url") {
		t.Fatalf("default wiring changed:\n%s", read())
	}
	if err := cx.Field("scope").Set("profile"); err == nil {
		t.Fatal("scope changed while connected")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "magpie.config.toml")); err == nil {
		t.Fatal("a profile was written by the default wiring")
	}
}

// Codex routed through magpie by the base URL alone (beside its ChatGPT
// sign-in, or for failover) reads as not wired, yet magpie keeps config.toml
// and its list there. "applies to" moving to the profile then made magpie
// stop looking at config.toml: its wiring stayed behind unmanaged and the
// row read disconnected, Codex on its own list (kakzyanaba on X).
func TestCodexScopeWaitsForBaseURLRouteToo(t *testing.T) {
	const routed = "openai_base_url = \"http://127.0.0.1:3425/backend-api/codex\"\n"
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, routed)
	cx := codex(home)
	if cx.Routed == nil || !cx.Routed() {
		t.Fatal("fixture is not routed by the base URL")
	}
	if err := cx.Field("scope").Set("profile"); err == nil {
		t.Fatal("scope changed while Codex is routed through magpie")
	}
	if codex(home).Path != filepath.Join(home, ".codex", "config.toml") || read() != routed {
		t.Fatalf("Codex moved off config.toml:\n%s", read())
	}
}
