package agent

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
)

const codexJoinedModelFixture = `model = "gpt-a"

[model_providers."user.relay"]
name = "my relay"
base_url = "https://example.invalid/v1"

[projects."/my/project"]
trust_level = "trusted"
`

func codexJoinedModel(t *testing.T, held bool) (*Agent, func() string, []string) {
	t.Helper()
	quietCodex(t, false, provider.CodexDaemonCheck{})
	codexReadersAre(t, "0.171.0")
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, codexJoinedModelFixture)
	writeFile(t, filepath.Join(home, ".codex", "models_cache.json"),
		`{"models":[{"slug":"gpt-a","display_name":"A","priority":1}]}`)
	agent := codex(home)
	if joined, err := agent.Join(); !joined || err != nil {
		t.Fatalf("join: %v, %v", joined, err)
	}
	at := here(home)
	marks := []string{at.key("codex.out"), at.key("codex.out_own"), at.key("codex.out_stand")}
	if held {
		codexUsedUp = func() bool { return true }
		if err := agent.Sync(); err != nil {
			t.Fatal(err)
		}
		if !agent.AppOut() || len(peek(marks...)) != len(marks) {
			t.Fatalf("expected stand-in takeover, got %v:\n%s", peek(marks...), read())
		}
		codexUsedUp = func() bool { return false }
	}
	return agent, read, marks
}

func TestCodexJoinedModelPreservesConcurrentConfig(t *testing.T) {
	for _, action := range []string{"pick", "recover", "already beside"} {
		t.Run(action, func(t *testing.T) {
			agent, read, marks := codexJoinedModel(t, action != "already beside")
			if action == "already beside" && !agent.Joined() {
				t.Fatal("fixture is not joined")
			}
			before := peek(marks...)
			want := "native-alias"
			apply := func() error { return agent.Apply("model", want) }
			if action == "recover" {
				want = "gpt-a"
				apply = agent.Sync
			}
			target, err := edit.Target(agent.Path)
			if err != nil {
				t.Fatal(err)
			}
			user := read() + "\n[agents]\nmax_threads = 9\n"
			var edited bool
			err = edit.Filtered(func(path string, body []byte) []byte {
				if path == target && !edited && !strings.Contains(string(body), `model_provider = "magpie"`) {
					writeFile(t, target, user)
					edited = true
				}
				return body
			}, apply)
			if !edited || !errors.Is(err, extension.ErrActionChanged) || read() != user {
				t.Errorf("stale selection: error %v, user edit preserved = %t", err, edited && read() == user)
			}
			if got := peek(marks...); !reflect.DeepEqual(got, before) {
				t.Errorf("stale selection changed takeover marks: %v → %v", before, got)
			}
			if t.Failed() {
				return
			}
			if err := apply(); err != nil {
				t.Fatal(err)
			}
			cfg := read()
			if !agent.Joined() || agent.AppOut() || len(peek(marks...)) != 0 ||
				!strings.Contains(cfg, `model = "`+want+`"`) || strings.Contains(cfg, "model_provider =") ||
				strings.Contains(cfg, "model_catalog_json") || !strings.Contains(cfg, "[model_providers.magpie]") ||
				!strings.Contains(cfg, `[model_providers."user.relay"]`) || !strings.Contains(cfg, `trust_level = "trusted"`) ||
				!strings.Contains(cfg, "max_threads = 9") {
				t.Fatalf("retry did not restore the joined model with the edited config:\n%s", cfg)
			}
		})
	}
}

func TestCodexJoinedModelRefusesInvalidCatalogShape(t *testing.T) {
	agent, read, marks := codexJoinedModel(t, true)
	before := peek(marks...)
	if err := edit.SetTOMLTop(agent.Path, edit.KV{Path: "model_catalog_json", Value: edit.Raw(`["user-models.json"]`)}); err != nil {
		t.Fatal(err)
	}
	config := read()
	target, err := edit.Target(agent.Path)
	if err != nil {
		t.Fatal(err)
	}
	var writes int
	err = edit.Filtered(func(path string, body []byte) []byte {
		if path == target {
			writes++
		}
		return body
	}, func() error {
		return agent.Apply("model", "native-alias")
	})
	if err == nil || writes != 0 || read() != config {
		t.Errorf("invalid catalog shape: error %v, %d settings writes, config preserved = %t", err, writes, read() == config)
	}
	if got := peek(marks...); !reflect.DeepEqual(got, before) {
		t.Errorf("refused selection changed takeover marks: %v → %v", before, got)
	}
}

func TestCodexJoinedModelRejectsRouteChangeBeforePlan(t *testing.T) {
	agent, read, marks := codexJoinedModel(t, true)
	before := peek(marks...)
	user := strings.Replace(read(), `model_provider = "magpie"`, `model_provider = "user.relay"`, 1)
	var edited bool
	codexUsedUp = func() bool {
		if !edited {
			writeFile(t, agent.Path, user)
			edited = true
		}
		return false
	}
	err := agent.Apply("model", "native-alias")
	if !edited || !errors.Is(err, extension.ErrActionChanged) || read() != user {
		t.Errorf("route changed before planning: error %v, user route preserved = %t", err, edited && read() == user)
	}
	if got := peek(marks...); !reflect.DeepEqual(got, before) {
		t.Errorf("route changed before planning moved takeover marks: %v → %v", before, got)
	}
}

func TestCodexJoinedModelKeepsIndependentCatalog(t *testing.T) {
	agent, read, _ := codexJoinedModel(t, true)
	const catalog = "custom-models.json"
	writeFile(t, filepath.Join(filepath.Dir(agent.Path), catalog), `{"models":[]}`)
	if err := edit.SetTOMLTop(agent.Path, edit.KV{Path: "model_catalog_json", Value: catalog}); err != nil {
		t.Fatal(err)
	}
	if err := agent.Apply("model", "native-alias"); err != nil {
		t.Fatal(err)
	}
	if cfg := read(); !agent.Joined() || !strings.Contains(cfg, `model_catalog_json = "`+catalog+`"`) {
		t.Fatalf("joined selection removed the user's catalog:\n%s", cfg)
	}
}
