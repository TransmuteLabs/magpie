package agent

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filememo"
)

func openCodeLegacyConfigFixture(t *testing.T, config string) *Agent {
	t.Helper()
	home, _ := codexHome(t, "", "")
	openCodeAt(t, "1.0.0")
	root := filepath.Join(home, ".config")
	writeFile(t, filepath.Join(root, "opencode", "opencode.jsonc"), config)
	return opencode(home, root)
}

func TestOpenCodeLegacySyncPlansBothProviderFormatsTogether(t *testing.T) {
	const input = `{
  "provider":{"magpie":{"name":"before v1","options":{"custom":"before"}}},
  "providers":{"magpie":{"name":"before v2","settings":{"custom":"before"}}}
}`
	t.Run("held config", func(t *testing.T) {
		oc := openCodeLegacyConfigFixture(t, input)
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
		oc := openCodeLegacyConfigFixture(t, input)
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
