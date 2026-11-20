package agent

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

const legacyOpenCodeConfig = `{
  // my preferences
  "theme": "quiet",
  "small_model": "magpie/fake/m1",
  "provider": {
    "other": {"name": "my provider"},
    "magpie": {"npm": "@ai-sdk/openai-compatible", "options": {"headers": {"X-Mine": "kept"}}},
  },
}
`

func openCodeConfigFixture(t *testing.T, version, config string) *Agent {
	t.Helper()
	home, _ := codexHome(t, "", "")
	openCodeAt(t, version)
	cfg := filepath.Join(home, ".config")
	writeFile(t, filepath.Join(cfg, "opencode", "opencode.jsonc"), config)
	return opencode(home, cfg)
}

func TestOpenCodeLinkedSyncPublishesV2CompatibilityTogether(t *testing.T) {
	oc := openCodeConfigFixture(t, "2.0.4", legacyOpenCodeConfig)
	target, err := edit.Target(oc.Path)
	if err != nil {
		t.Fatal(err)
	}
	var proposals []string
	if err := edit.Filtered(func(path string, data []byte) []byte {
		if path == target {
			proposals = append(proposals, string(data))
		}
		return data
	}, oc.Sync); err != nil {
		t.Fatal(err)
	}
	if len(proposals) == 0 {
		t.Fatal("expected the v2 compatibility migration")
	}
	for _, proposal := range proposals {
		if strings.Contains(proposal, `"small_model"`) || strings.Contains(proposal, `"npm": "@ai-sdk/openai-compatible"`) ||
			!strings.Contains(proposal, `"providers"`) || !strings.Contains(proposal, `"title"`) ||
			!strings.Contains(proposal, `// my preferences`) || !strings.Contains(proposal, `"other": {"name": "my provider"}`) {
			t.Fatalf("published an intermediate or lossy migration:\n%s", proposal)
		}
	}
	for key, want := range map[string]string{
		openCodeTitleKey:                      "magpie/fake/m1",
		openCodeNativeKey + ".headers.X-Mine": "kept",
		"provider.other.name":                 "my provider",
		"theme":                               "quiet",
	} {
		if got, _ := edit.GetJSON(oc.Path, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestOpenCodeLinkedSyncPreservesConcurrentEdit(t *testing.T) {
	oc := openCodeConfigFixture(t, "2.0.4", legacyOpenCodeConfig)
	target, err := edit.Target(oc.Path)
	if err != nil {
		t.Fatal(err)
	}
	user := strings.Replace(legacyOpenCodeConfig, `"quiet"`, `"mine"`, 1)
	var edited bool
	err = edit.Filtered(func(path string, data []byte) []byte {
		if path == target && !edited {
			writeFile(t, path, user)
			edited = true
		}
		return data
	}, oc.Sync)
	if !edited || !errors.Is(err, extension.ErrActionChanged) || readFile(oc.Path) != user {
		t.Fatalf("edited = %t, error = %v, config = %q", edited, err, readFile(oc.Path))
	}
}

func TestOpenCodeLinkedApplyPublishesTitleAndRemovesLegacyModelTogether(t *testing.T) {
	const input = `{"small_model":"mine/old","agents":{"title":{"prompt":"keep this"}}}`
	oc := openCodeConfigFixture(t, "2.0.4", input)
	target, err := edit.Target(oc.Path)
	if err != nil {
		t.Fatal(err)
	}
	var proposals []string
	if err := edit.Filtered(func(path string, data []byte) []byte {
		if path == target {
			proposals = append(proposals, string(data))
		}
		return data
	}, func() error { return oc.Apply("small", "mine/new") }); err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 || strings.Contains(proposals[0], `"small_model"`) || oc.Values()["small"] != "mine/new" {
		t.Fatalf("proposed %q, small = %q", proposals, oc.Values()["small"])
	}
	if prompt, _ := edit.GetJSON(oc.Path, "agents.title.prompt"); prompt != "keep this" {
		t.Fatalf("title prompt = %q", prompt)
	}
}

func TestOpenCodeLinkedApplyUsesWSLDistroConfigAndVersion(t *testing.T) {
	home, _ := codexHome(t, "", "")
	openCodeAt(t, "1.3.9")
	distro := t.TempDir()
	distroPath := filepath.Join(distro, ".config", "opencode", "opencode.jsonc")
	const input = `{"small_model":"mine/old","agents":{"title":{"prompt":"keep this"}}}`
	writeFile(t, distroPath, input)
	localPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	writeFile(t, localPath, input)
	oc := opencodeIn(place{
		home: distro, id: "opencode@wsl:Ubuntu", version: "2.0.4",
		spell: func(path string) string { return filepath.Base(path) },
	})
	if err := oc.Apply("small", "mine/new"); err != nil {
		t.Fatal(err)
	}
	if got, _ := edit.GetJSON(distroPath, openCodeTitleKey); got != "mine/new" || readFile(localPath) != input {
		t.Fatalf("title model = %q, local config = %q", got, readFile(localPath))
	}
	if _, ok := edit.GetJSON(distroPath, "small_model"); ok {
		t.Fatal("the v2 distro kept the legacy small model")
	}
}
