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
	"github.com/yetone/magpie/internal/filememo"
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

func TestOpenCodeLinkedPrimaryPublishesProviderWithModel(t *testing.T) {
	const input = "{\n  // keep my theme\n  \"theme\": \"mine\",\n}\n"
	oc := openCodeConfigFixture(t, "2.0.4", input)
	reasonCatalog(t)
	target, err := edit.Target(oc.Path)
	if err != nil {
		t.Fatal(err)
	}
	var proposals []string
	observe := func(path string, data []byte) []byte {
		if path == target {
			proposals = append(proposals, string(data))
		}
		return data
	}
	if err := edit.Filtered(observe, func() error { return oc.Apply("model", "magpie/think/deep") }); err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 || !strings.Contains(proposals[0], `"magpie/think/deep"`) ||
		!strings.Contains(proposals[0], `"providers"`) || !strings.Contains(proposals[0], "// keep my theme") {
		t.Fatalf("model and provider published separately or lost user settings: %q", proposals)
	}
	nativeOnly(t, oc.Path, "think/deep")
}

func TestOpenCodeLinkedDisconnectPublishesProviderAndModelTogether(t *testing.T) {
	const input = "{\n  // keep my theme\n  \"theme\": \"mine\",\n}\n"
	oc := openCodeConfigFixture(t, "2.0.4", input)
	reasonCatalog(t)
	if err := oc.Apply("model", "magpie/think/deep"); err != nil {
		t.Fatal(err)
	}
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
	}, oc.Disconnect); err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 || strings.Contains(proposals[0], `"model"`) ||
		strings.Contains(proposals[0], `"magpie"`) || !strings.Contains(proposals[0], "// keep my theme") {
		t.Fatalf("model and provider disconnected separately or lost user settings: %q", proposals)
	}
}

func TestOpenCodeLinkedPrimaryWaitsForDiscoverySidecar(t *testing.T) {
	oc := openCodeConfigFixture(t, "2.2.6", `{"model":"mine/old"}`)
	reasonCatalog(t)
	path := openCodePluginPath(oc.Path)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := oc.Apply("model", "magpie/think/deep"); err == nil {
		t.Fatal("expected a discovery sidecar error")
	}
	if model := oc.Values()["model"]; model != "mine/old" {
		t.Fatalf("selected %q despite the sidecar failure", model)
	}
}

func TestOpenCodeLinkedDiscoveryKeepsNewReplyLimit(t *testing.T) {
	oc := openCodeConfigFixture(t, "2.2.6", `{}`)
	reasonCatalog(t)
	if err := oc.Apply("model", "magpie/rsn/plan"); err != nil {
		t.Fatal(err)
	}
	key := openCodeNativeKey + ".models.rsn/plan.limit.output"
	if err := edit.SetJSON(oc.Path, edit.KV{Path: key, Value: 1234}); err != nil {
		t.Fatal(err)
	}
	if err := oc.Sync(); err != nil {
		t.Fatal(err)
	}
	if output, _ := edit.GetJSON(oc.Path, key); output != "1234" {
		t.Fatalf("user reply limit = %q", output)
	}
}

func TestOpenCodeLinkedCatalogSyncPreservesConcurrentEdit(t *testing.T) {
	const input = `{"theme":"old","providers":{"magpie":{"settings":{"baseURL":"http://localhost:1/v1"}}}}`
	oc := openCodeConfigFixture(t, "2.0.4", input)
	reasonCatalog(t)
	target, err := edit.Target(oc.Path)
	if err != nil {
		t.Fatal(err)
	}
	user := strings.Replace(input, `"old"`, `"mine"`, 1)
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

func TestOpenCodeLegacySyncPlansBothProviderFormatsTogether(t *testing.T) {
	const input = `{
  "provider":{"magpie":{"name":"before v1","options":{"custom":"before"}}},
  "providers":{"magpie":{"name":"before v2","settings":{"custom":"before"}}}
}`
	t.Run("held config", func(t *testing.T) {
		oc := openCodeConfigFixture(t, "1.0.0", input)
		reasonCatalog(t)
		target, err := edit.Target(oc.Path)
		if err != nil {
			t.Fatal(err)
		}
		release := filememo.Hold()
		defer release()
		if _, ok := edit.GetJSON(oc.Path, "provider.magpie"); !ok {
			t.Fatal("legacy provider missing from fixture")
		}
		writeFile(t, oc.Path, strings.ReplaceAll(input, `"custom":"before"`, `"custom":"mine"`))
		var writes int
		err = edit.Filtered(func(path string, data []byte) []byte {
			if path == target {
				writes++
				if strings.Contains(string(data), "before v1") || strings.Contains(string(data), "before v2") {
					t.Error("Sync published only one of the provider formats")
				}
			}
			return data
		}, oc.Sync)
		if err != nil || writes != 1 {
			t.Fatalf("Sync made %d writes: %v", writes, err)
		}
		for _, key := range []string{"provider.magpie.options.custom", "providers.magpie.settings.custom"} {
			if got, _ := edit.GetJSON(oc.Path, key); got != "mine" {
				t.Errorf("Sync lost the current value at %s: %q", key, got)
			}
		}
	})
	t.Run("concurrent edit", func(t *testing.T) {
		oc := openCodeConfigFixture(t, "1.0.0", input)
		reasonCatalog(t)
		key := outputsWantedKey(oc.Path)
		target, err := edit.Target(oc.Path)
		if err != nil {
			t.Fatal(err)
		}
		user := strings.ReplaceAll(input, `"custom":"before"`, `"custom":"mine"`)
		var edited bool
		err = edit.Filtered(func(path string, data []byte) []byte {
			if path == target && !edited {
				writeFile(t, path, user)
				edited = true
			}
			return data
		}, oc.Sync)
		if !edited || !errors.Is(err, extension.ErrActionChanged) || readFile(oc.Path) != user || unstashPeek(key) != "" {
			t.Fatalf("Sync published over an edit or saved stale output limits: edited=%t, err=%v, config=%s, limits=%s",
				edited, err, readFile(oc.Path), unstashPeek(key))
		}
	})
}

func TestOpenCodeLinkedConflictLeavesOutputHistory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		pick    bool
	}{
		{name: "listed_sync", version: "2.0.4"},
		{name: "discovery_pick", version: "2.2.6", pick: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const input = `{"theme":"old","providers":{"magpie":{"settings":{"baseURL":"http://localhost:1/v1"}}}}`
			oc := openCodeConfigFixture(t, tc.version, input)
			reasonCatalog(t)
			target, err := edit.Target(oc.Path)
			if err != nil {
				t.Fatal(err)
			}
			apply := oc.Sync
			if tc.pick {
				apply = func() error { return oc.Apply("model", "magpie/rsn/plan") }
			}
			user := strings.Replace(input, `"old"`, `"mine"`, 1)
			key := outputsWantedKey(oc.Path)
			before := unstashPeek(key)
			var edited bool
			err = edit.Filtered(func(path string, data []byte) []byte {
				if path == target && !edited {
					writeFile(t, path, user)
					edited = true
				}
				return data
			}, apply)
			if !edited || !errors.Is(err, extension.ErrActionChanged) || readFile(oc.Path) != user {
				t.Fatalf("edited = %t, error = %v, config = %q", edited, err, readFile(oc.Path))
			}
			if after := unstashPeek(key); after != before {
				t.Fatalf("conflicted action changed output history from %q to %q", before, after)
			}
			if err := apply(); err != nil {
				t.Fatal(err)
			}
			var saved map[string][]int
			after := unstashPeek(key)
			if err := json.Unmarshal([]byte(after), &saved); err != nil || len(saved["rsn/plan"]) == 0 {
				t.Fatalf("successful action did not record the output history: %q (%v)", after, err)
			}
		})
	}
}

func TestOpenCodeLinkedDiscoveryPublishesProviderAndCatalogChangeTogether(t *testing.T) {
	oc := openCodeConfigFixture(t, "2.0.4", `{}`)
	reasonCatalog(t)
	if err := oc.Apply("model", "magpie/think/deep"); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetJSON(oc.Path,
		edit.KV{Path: openCodeNativeKey + ".settings.baseURL", Value: "http://localhost:1/v1"},
		edit.KV{Path: openCodeNativeKey + ".settings.timeout", Value: 12348},
	); err != nil {
		t.Fatal(err)
	}
	target, err := edit.Target(oc.Path)
	if err != nil {
		t.Fatal(err)
	}
	openCodeAt(t, "2.2.6")
	var proposals []string
	if err := edit.Filtered(func(path string, data []byte) []byte {
		if path == target {
			proposals = append(proposals, string(data))
		}
		return data
	}, oc.Sync); err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 || strings.Contains(proposals[0], `"models"`) ||
		!strings.Contains(proposals[0], gatewayV1()) || !strings.Contains(proposals[0], "12348") {
		t.Fatalf("provider and discovery catalog published separately or lost user settings: %q", proposals)
	}
	if !strings.HasPrefix(pluginOf(oc.Path), openCodePluginMark) {
		t.Fatal("missing discovery sidecar")
	}
}
