package sessions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSourceBindingInspectionRejectsMalformedAuthority(t *testing.T) {
	setup(t)
	path := sourceBindingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, binding := range []SessionSourceBinding{
		{Plugin: "fixture", Source: "history", Agent: "omp", Identity: strings.Repeat("z", 64)},
		{Plugin: "fixture", Source: "history", Agent: "omp", Identity: strings.Repeat("A", 64)},
		{Plugin: "fixture", Source: "history", Agent: "foreign", Identity: strings.Repeat("a", 64)},
	} {
		body, err := json.Marshal([]SessionSourceBinding{binding})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		status := InspectSessionSources(context.Background())
		if len(status.Errors) == 0 || len(status.Bindings) != 0 {
			t.Fatal("source inspection treated malformed authority as a usable binding", status)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(body) {
			t.Fatal("failed inspection replaced malformed binding bytes", err)
		}
	}
}

func TestSourceUnknownFormatNeverUsesClaudeParser(t *testing.T) {
	setup(t)
	sourceView.Store(&sessionSourceView{epoch: sourceEpoch.Load(), agents: map[string]bool{"fixture-source": true}})
	defer sourceView.Store(nil)
	if parserFor("fixture-source").claude || parserFor("fixture-source").line != nil {
		t.Fatal("registered SDK source silently fell through to Claude")
	}
	path := filepath.Join(t.TempDir(), "source.jsonl")
	body := []byte("{\"type\":\"user\",\"sessionId\":\"secret\",\"cwd\":\"/secret\",\"message\":{\"content\":\"must not decode as Claude\"}}\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	item := file{agent: "fixture-source", key: "fixture-source:source", path: path, main: true}
	if !stat(&item) {
		t.Fatal("fixture missing")
	}
	state := parseRead(item, nil, nil)
	if state.ID != "" || state.Cwd != "" || state.Title != "" {
		t.Fatal("unknown codec was decoded using a native format", state)
	}
	if !parserFor("claude").claude {
		t.Fatal("SDK source quarantine changed native Claude default")
	}
}

func TestSourceRootSlotsRejectForeignOwners(t *testing.T) {
	setup(t)
	for _, slot := range []string{"claude.projects", "codex.sessions", "codex.archived-sessions", "omp.sessions", "devin.file-sessions"} {
		if _, err := sourceStoreRoots(sdk.SessionSourceDescriptor{Agent: "fixture-source"},
			sdk.SessionSourceStore{Slot: "main", NativeRootSlot: slot}); err == nil {
			t.Fatal("foreign native root was granted", slot)
		}
	}
}
