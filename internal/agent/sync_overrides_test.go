package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filememo"
)

func TestAgentCatalogSyncKeepsFreshOverrides(t *testing.T) {
	for _, test := range []struct {
		agent    string
		open     func(string) *Agent
		field    string
		before   string
		oldValue string
		newValue string
	}{
		{"Ante", ante, "http_headers.X-Owner", `{"http_headers":{"X-Owner":"previous"}}`, "previous", "latest"},
		{"Crush", func(home string) *Agent { return crush(home, filepath.Join(home, ".config")) },
			"headers.X-Owner", `{"headers":{"X-Owner":"previous"}}`, "previous", "latest"},
		{"fx", fx, "tool_choice_mode", `{"tool_choice_mode":"send"}`, "send", "omit"},
		{"MothX", mothx, "thinkingFormat", `{"thinkingFormat":"openai"}`, "openai", "anthropic"},
	} {
		t.Run(test.agent, func(t *testing.T) {
			agent := test.open(syncHome(t))
			config := `{"providers":{"magpie":` + test.before + `}}`
			writeFile(t, agent.Path, config)
			settled := time.Now().Add(-time.Minute)
			if err := os.Chtimes(agent.Path, settled, settled); err != nil {
				t.Fatal(err)
			}
			unhold := filememo.Hold()
			defer unhold()
			key := "providers.magpie." + test.field
			if value, _ := edit.GetJSON(agent.Path, key); value != test.oldValue {
				t.Fatalf("fixture %s = %q", key, value)
			}
			writeFile(t, agent.Path, strings.Replace(config, `"`+test.oldValue+`"`, `"`+test.newValue+`"`, 1))
			if value, _ := edit.GetJSON(agent.Path, key); value != test.oldValue {
				t.Fatalf("fixture did not hold previous %s: %q", key, value)
			}
			if err := agent.Sync(); err != nil {
				t.Fatal(err)
			}
			unhold()
			if value, _ := edit.GetJSON(agent.Path, key); value != test.newValue {
				t.Errorf("Sync overwrote %s with %q, want %q", key, value, test.newValue)
			}
		})
	}
}
