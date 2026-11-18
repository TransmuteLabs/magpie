package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every built-in agent field's label has its Chinese in internal/tui's zh map
// (i18n.go), which the TUI shows it through. internal/tui's
// TestEveryWordHasZh checks it too, but a change that adds a field runs
// this package, and a new label with no Chinese left that test red on
// main (e9fcc6229 for "permission mode", 11-15; "subagent pool" from
// 618bfc1a9, 11-17), each found after the push. tui imports agent, so
// this reads its files.
func TestEveryFieldLabelHasTUIZh(t *testing.T) {
	dir := filepath.Join("..", "tui")
	zh, err := os.ReadFile(filepath.Join(dir, "i18n.go"))
	if err != nil {
		t.Fatal(err)
	}
	tests, err := os.ReadFile(filepath.Join(dir, "i18n_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`var keepAsIs = map\[string\]bool\{([^}]*)\}`).FindSubmatch(tests)
	if m == nil {
		t.Fatal("internal/tui/i18n_test.go has no keepAsIs line")
	}
	keep := map[string]bool{}
	for _, k := range regexp.MustCompile(`"([^"]*)":`).FindAllSubmatch(m[1], -1) {
		keep[string(k[1])] = true
	}
	home, cfg := homes()
	missing, n := untranslatedNativeAgentFields(builtins(home, cfg), keep, zh)
	for _, message := range missing {
		t.Error(message)
	}
	if n < 30 {
		t.Fatalf("read %d labels", n)
	}
}

func untranslatedNativeAgentFields(agents []*Agent, keep map[string]bool, zh []byte) ([]string, int) {
	n := 0
	var missing []string
	seen := map[string]bool{}
	for _, a := range agents {
		for _, f := range a.Fields {
			if keep[f.Label] || seen[f.Label] || strings.TrimSpace(f.Label) == "" {
				continue
			}
			seen[f.Label] = true
			n++
			q := regexp.QuoteMeta(`"` + f.Label + `"`)
			if !regexp.MustCompile(`(?m)^\s*` + q + `:\s*"`).Match(zh) {
				missing = append(missing, fmt.Sprintf("%s's field %q (label %q) has no Chinese in internal/tui/i18n.go's zh map", a.ID, f.Key, f.Label))
			}
		}
	}
	return missing, n
}
