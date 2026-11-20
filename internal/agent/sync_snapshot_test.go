package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/edit"
)

func TestJSONCatalogSyncKeepsConcurrentSettings(t *testing.T) {
	const before = "{\"provider\":{\"models\":[\"old\"],\"local\":\"before\"}}\n"
	const user = "{\"provider\":{\"models\":[\"old\"],\"local\":\"after\"}}\n"
	for _, name := range []string{"modified", "removed"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "models.json")
			if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
				t.Fatal(err)
			}
			err := syncJSON(path, "provider", func() any {
				var err error
				if name == "removed" {
					err = os.Remove(path)
				} else {
					err = os.WriteFile(path, []byte(user), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
				return map[string]any{"models": []string{"new"}, "local": "before"}
			})
			body, readErr := os.ReadFile(path)
			if name == "removed" {
				if !os.IsNotExist(readErr) {
					t.Errorf("Sync recreated the removed file: %s, %v", body, readErr)
				}
			} else if readErr != nil || string(body) != user {
				t.Errorf("Sync lost the concurrent edit: %s, %v", body, readErr)
			}
			if err == nil {
				t.Error("Sync accepted a stale settings snapshot")
			}
		})
	}
}

func TestJSONCatalogSyncPreservesJSONCAndTypes(t *testing.T) {
	const prefix = "\xef\xbb\xbf{\r\n  // keep this setting\r\n"
	const original = prefix + "  \"name\": \"true\",\r\n  \"provider\": { \"b\": 2, \"a\": 1 },\r\n}\r\n"
	path := filepath.Join(t.TempDir(), "models.jsonc")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	value := func() any { return map[string]int{"a": 1, "b": 2} }
	if err := syncJSON(path, "missing", func() any { t.Fatal("missing setting evaluated"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := syncJSON(path, "provider", value); err != nil {
		t.Fatal(err)
	}
	if err := syncJSON(path, "name", func() any { return "true" }); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != original {
		t.Fatalf("unchanged value rewrote JSONC: %q, %v", body, err)
	}
	if err := syncJSON(path, "provider", func() any { return map[string]int{"a": 3, "b": 2} }); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(body, []byte(prefix)) || bytes.Contains(bytes.ReplaceAll(body, []byte("\r\n"), nil), []byte("\n")) {
		t.Errorf("Sync discarded JSONC formatting: %q, %v", body, err)
	}
	if err := syncJSON(path, "name", func() any { return true }); err != nil {
		t.Fatal(err)
	}
	if raw, _ := edit.GetJSONRaw(path, "name"); raw != "true" {
		t.Errorf("Sync treated JSON string and bool as equivalent: %s", raw)
	}
}
