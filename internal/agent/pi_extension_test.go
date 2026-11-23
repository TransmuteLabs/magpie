package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtinpi "github.com/yetone/magpie/internal/extension/builtin/pi"
	"github.com/yetone/magpie/internal/provider"
)

// piExtensionHome is a sandbox home with a provider that decides
// (typesafe/jev) and one that draws (gpt-image-2, which takes images too).
func piExtensionHome(t *testing.T) string {
	t.Helper()
	home := syncHome(t)
	// no request reaches these: the extension only names them
	if err := provider.Save(provider.Provider{ID: "mixed", Name: "Mixed", Key: "k", Chat: "http://127.0.0.1:9/v1", Decide: "http://127.0.0.1:9/v1", Models: []string{"deepseek-v4.1-flash", "typesafe/jev"}}); err != nil {
		t.Fatal(err)
	}
	art := provider.Provider{ID: "art", Name: "Art", Responses: "http://127.0.0.1:9/v1", Key: "k"}
	if err := provider.Save(art); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("art", art.Responses, []catalog.Model{{ID: "text"}, {ID: "gpt-image-2", Name: "GPT Image 2", Draws: true, Images: true}}); err != nil {
		t.Fatal(err)
	}
	return home
}

type piExtensionData struct {
	BaseURL     string             `json:"baseUrl"`
	APIKey      string             `json:"apiKey"`
	Classifiers []piExtensionModel `json:"classifiers"`
	Images      []piExtensionModel `json:"images"`
}

// readPiExtension is the data in the extension magpie wrote at path.
func readPiExtension(t *testing.T, path string) piExtensionData {
	t.Helper()
	src := readFile(path)
	if !strings.HasPrefix(src, builtinpi.SidecarMark) {
		t.Fatalf("%s is not magpie's: %.200s", path, src)
	}
	_, rest, ok := strings.Cut(src, "const MAGPIE = ")
	line, _, _ := strings.Cut(rest, ";\n")
	var d piExtensionData
	if !ok || json.Unmarshal([]byte(line), &d) != nil {
		t.Fatalf("no MAGPIE data in %s: %.400s", path, rest)
	}
	return d
}

func hasModel(ms []piExtensionModel, id string) (piExtensionModel, bool) {
	for _, m := range ms {
		if m.ID == id {
			return m, true
		}
	}
	return piExtensionModel{}, false
}

func TestPiExtensionFailureKeepsWiring(t *testing.T) {
	for _, action := range []string{"sync", "pick", "reset", "protocol", "live-sync"} {
		t.Run(action, func(t *testing.T) {
			home := syncHome(t)
			client := pi(home)
			models := filepath.Join(client.Dir, "models.json")
			settings := `{"defaultProvider":"magpie","defaultModel":"relay/old","enabledModels":["magpie/relay/old"]}`
			catalog := `{"providers":{"magpie":{"models":[{"id":"relay/old"}]}}}`
			if action == "live-sync" {
				if err := client.Apply("model", "magpie/relay/glm-4.6"); err != nil {
					t.Fatal(err)
				}
				catalog = readFile(models)
				settings = strings.TrimSuffix(settings, "}") + `,"packages":["npm:pi-magpie"]}`
			}
			writeFile(t, client.Path, settings)
			writeFile(t, models, catalog)
			if err := os.MkdirAll(piExtensionPath(client.Dir), 0o700); err != nil {
				t.Fatal(err)
			}
			operate := client.Sync
			switch action {
			case "pick":
				operate = func() error { return client.Pick("model", "magpie/relay/glm-4.6") }
			case "reset":
				operate = func() error { return client.Field("model").Set("") }
			case "protocol":
				operate = func() error { return client.Pick("protocol", "openai-responses") }
			}
			history := stashShared()
			if err := operate(); err == nil {
				t.Fatal("an invalid managed extension path was accepted")
			}
			if readFile(client.Path) != settings || readFile(models) != catalog {
				t.Errorf("a failed extension operation updated Pi settings or catalog")
			}
			current := stashShared()
			if current[apisWantedKey(models)] != history[apisWantedKey(models)] ||
				current[outputsWantedKey(models)] != history[outputsWantedKey(models)] ||
				current[piProtocolKey(here(home))] != history[piProtocolKey(here(home))] {
				t.Error("a failed extension operation updated catalog history")
			}
			if _, err := os.Lstat(models + ".before-pi-magpie"); !os.IsNotExist(err) {
				t.Errorf("a failed takeover left a backup: %v", err)
			}
		})
	}
}

func TestPiExtensionRemovesOnlyTheManagedAlias(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			home := piExtensionHome(t)
			client := pi(home)
			path := piExtensionPath(client.Dir)
			pick := func() error { return client.Pick("model", "magpie/mixed/deepseek-v4.1-flash") }
			if err := pick(); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(client.Dir, "kept.ts")
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			link := os.Symlink
			if kind == "hardlink" {
				link = os.Link
			}
			if err := link(target, path); err != nil {
				t.Skipf("%s unavailable: %v", kind, err)
			}
			if err := provider.Delete("art"); err != nil {
				t.Fatal(err)
			}
			if err := client.Sync(); err != nil {
				t.Fatal(err)
			}
			left, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			right, err := os.Stat(target)
			if err != nil || !os.SameFile(left, right) {
				t.Fatalf("Sync changed the alias target: %v", err)
			}
			saved := readFile(target)
			if extension := readPiExtension(t, target); len(extension.Images) != 0 {
				t.Fatal("Sync did not update the extension through the alias")
			}
			if err := client.Field("model").Set(""); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) || readFile(target) != saved {
				t.Errorf("reset did not remove only the managed alias: %v", err)
			}
		})
	}
}

func TestPiExtensionResetKeepsReplacedAlias(t *testing.T) {
	client := pi(piExtensionHome(t))
	if err := client.Pick("model", "magpie/mixed/deepseek-v4.1-flash"); err != nil {
		t.Fatal(err)
	}
	path := piExtensionPath(client.Dir)
	target := filepath.Join(client.Dir, "kept.ts")
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	replacement := path + ".replacement"
	if err := os.Symlink(target, replacement); err != nil {
		t.Fatal(err)
	}
	settings, err := edit.Target(client.Path)
	if err != nil {
		t.Fatal(err)
	}
	models := filepath.Join(client.Dir, "models.json")
	config, catalog, script := readFile(settings), readFile(models), readFile(target)
	replaced := false
	err = edit.Filtered(func(path string, body []byte) []byte {
		if path == settings && !replaced {
			if err := os.Rename(replacement, piExtensionPath(client.Dir)); err != nil {
				t.Fatal(err)
			}
			replaced = true
		}
		return body
	}, func() error { return client.Field("model").Set("") })
	if !replaced || !errors.Is(err, extension.ErrActionChanged) {
		t.Fatalf("reset accepted a replaced alias: replaced=%t, err=%v", replaced, err)
	}
	if readFile(settings) != config || readFile(models) != catalog || readFile(target) != script || readFile(path) != script {
		t.Fatal("reset changed wiring or removed the replacement")
	}
}

// Pi's codemode reaches classifier and image models, which models.json
// can't define (wangbinyq on Discord): picking a model of magpie's writes
// extensions/magpie.ts with magpie's decision and image models, and
// taking Pi off magpie removes it.
func TestPiExtensionListsDecisionAndImageModels(t *testing.T) {
	home := piExtensionHome(t)
	a := pi(home)
	path := piExtensionPath(a.Dir)
	if err := a.Field("model").Set(magpieID + "/mixed/deepseek-v4.1-flash"); err != nil {
		t.Fatal(err)
	}
	d := readPiExtension(t, path)
	if d.BaseURL != here(home).v1() {
		t.Errorf("baseUrl = %q, want %q", d.BaseURL, here(home).v1())
	}
	if want := mustGet(t, filepath.Join(a.Dir, "models.json"), "providers.magpie.apiKey"); d.APIKey != strings.Trim(want, `"`) {
		t.Errorf("apiKey = %q, models.json's is %s", d.APIKey, want)
	}
	if _, ok := hasModel(d.Classifiers, "mixed/typesafe/jev"); !ok {
		t.Errorf("classifiers = %+v, want mixed/typesafe/jev", d.Classifiers)
	}
	if _, ok := hasModel(d.Classifiers, "mixed/deepseek-v4.1-flash"); ok {
		t.Errorf("a chat model listed as a classifier: %+v", d.Classifiers)
	}
	img, ok := hasModel(d.Images, "art/gpt-image-2")
	if !ok {
		t.Fatalf("images = %+v, want art/gpt-image-2", d.Images)
	}
	if strings.Join(img.Input, ",") != "text,image" || img.Name != "GPT Image 2 · Art" {
		t.Errorf("image model = %+v", img)
	}
	if !strings.Contains(readFile(path), `pi.registerProvider("magpie-systemone"`) || !strings.Contains(readFile(path), `"typesafe-system-one"`) {
		t.Errorf("extension source: %.300s", readFile(path))
	}

	if err := a.Field("model").Set(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("extension left after Pi went off magpie: %v", err)
	}
}

// The catalog sync rewrites the extension as magpie's models change, and
// removes it once models.json has no provider magpie.
func TestPiExtensionFollowsSync(t *testing.T) {
	home := piExtensionHome(t)
	a := pi(home)
	path := piExtensionPath(a.Dir)
	if err := a.Field("model").Set(magpieID + "/mixed/deepseek-v4.1-flash"); err != nil {
		t.Fatal(err)
	}
	// the one provider that draws removed: no image model is listed
	if err := provider.Delete("art"); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if d := readPiExtension(t, path); len(d.Images) != 0 || len(d.Classifiers) == 0 {
		t.Errorf("after the drawing provider went: %+v", d)
	}
	models := filepath.Join(a.Dir, "models.json")
	if err := edit.DelJSON(models, "providers.magpie"); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("extension left with no provider magpie: %v", err)
	}
}

// A magpie.ts the user wrote themselves is theirs: never written over,
// never removed.
func TestPiExtensionLeavesTheUsersOwn(t *testing.T) {
	home := piExtensionHome(t)
	a := pi(home)
	path := piExtensionPath(a.Dir)
	os.MkdirAll(filepath.Dir(path), 0o755)
	own := "// my own magpie helpers\nexport default function (pi) {}\n"
	os.WriteFile(path, []byte(own), 0o644)
	if err := a.Field("model").Set(magpieID + "/mixed/deepseek-v4.1-flash"); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(path); got != own {
		t.Errorf("the user's file written over: %.200s", got)
	}
	if err := a.Field("model").Set(""); err != nil {
		t.Fatal(err)
	}
	if got := readFile(path); got != own {
		t.Errorf("the user's file removed or changed: %q", got)
	}
}

// OmO keeps Pi's files but may not load Pi's extensions: it gets none.
func TestPiExtensionPiOnly(t *testing.T) {
	home := piExtensionHome(t)
	a := omo(home)
	if err := a.Field("model").Set(magpieID + "/mixed/deepseek-v4.1-flash"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(piExtensionPath(a.Dir)); !os.IsNotExist(err) {
		t.Errorf("OmO got Pi's extension: %v", err)
	}
}

// #7521: a classifier that takes images (Google's gemini-decision-1) is
// registered with them, so Pi sends it a context's images; Jev, text
// alone, stays text. A magpie.ts written before, with every classifier
// text alone, is written again on the next sync.
func TestPiExtensionClassifierInput(t *testing.T) {
	home := piExtensionHome(t)
	// no request reaches it: the extension only names it
	if err := provider.Save(provider.Provider{ID: "gem", Name: "Gem", Gemini: provider.AIStudioGemini, Key: "AIza-test"}); err != nil {
		t.Fatal(err)
	}
	a := pi(home)
	path := piExtensionPath(a.Dir)
	if err := a.Field("model").Set(magpieID + "/mixed/deepseek-v4.1-flash"); err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		d := readPiExtension(t, path)
		for id, want := range map[string]string{"gem/" + provider.GoogleDecision: "text,image", "mixed/typesafe/jev": "text"} {
			m, ok := hasModel(d.Classifiers, id)
			if !ok {
				t.Fatalf("%s: classifiers = %+v, want %s", when, d.Classifiers, id)
			}
			if got := strings.Join(m.Input, ","); got != want {
				t.Errorf("%s: %s input = %q, want %q", when, id, got, want)
			}
		}
		// magpie's /v1/systemone takes images as "images", where Pi's
		// default for a host not OpenRouter's is TypeSafe's assets
		if src := readFile(path); strings.Contains(src, `name: m.name, input: ["text"]`) || !strings.Contains(src, "input: m.input") || !strings.Contains(src, `imageInput: "images"`) {
			t.Errorf("%s: magpie-systemone's models take no input of their own: %.1500s", when, src)
		}
	}
	check("written")

	// the file as magpie wrote it before: classifiers with no input, and
	// magpie-systemone's models text alone
	old := strings.Replace(readFile(path), "input: m.input ?? [\"text\"]", "input: [\"text\"]", 1)
	old = strings.Replace(old, `imageInput: "images"`, `imageInput: "assets"`, 1)
	old = strings.ReplaceAll(old, `,"input":["text","image"]}`, `}`)
	old = strings.ReplaceAll(old, `,"input":["text"]}`, `}`)
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	check("after a sync over the old file")
}
