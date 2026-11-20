package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/settings"
)

func mutationHistoryBinding(t *testing.T, agent string) {
	t.Helper()
	body, err := json.Marshal([]SessionSourceBinding{{
		Plugin: "fixture", Source: "history", Agent: agent, Identity: strings.Repeat("a", 64),
	}})
	if err != nil {
		t.Fatal(err)
	}
	writeHistoryAuthority(t, sourceBindingsPath(), body)
}

func TestClaudeRelocationRefusesDelegatedNativeHistory(t *testing.T) {
	for _, phase := range []string{"preview", "commit", "staged"} {
		t.Run(phase, func(t *testing.T) {
			setup(t)
			root, source, request := relocationFixture(t)
			t.Setenv("CLAUDE_CONFIG_DIR", root)
			original := filepath.Join(source, mgCC+".jsonl")
			before, err := os.ReadFile(original)
			if err != nil {
				t.Fatal(err)
			}
			if phase != "preview" {
				out, err := RelocateClaudeProject(request)
				if err != nil || out.Token == "" {
					t.Fatalf("native preview: %+v, %v", out, err)
				}
				request.Token = out.Token
			}
			if phase == "staged" {
				oldHook := sourceBeforePublish
				sourceBeforePublish = func() { mutationHistoryBinding(t, "claude") }
				t.Cleanup(func() { sourceBeforePublish = oldHook })
			} else {
				mutationHistoryBinding(t, "claude")
			}
			out, err := RelocateClaudeProject(request)
			if !errors.Is(err, extension.ErrActionChanged) || out.Backup != "" || out.Token != "" {
				t.Errorf("delegated project relocation escaped owner check: %+v, %v", out, err)
			}
			after, err := os.ReadFile(original)
			if err != nil || !bytes.Equal(after, before) {
				t.Errorf("original was changed: %v", err)
			}
			name, err := claudeProjectName(request.To)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, "projects", name)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("destination was published: %v", err)
			}
			if entries, err := os.ReadDir(filepath.Join(root, "magpie-relocations")); len(entries) != 0 || err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("staged files survived refusal: %v, %v", entries, err)
			}
		})
	}
}

func TestMoveCodexProviderRechecksOwnerAfterSelection(t *testing.T) {
	_, codex := manageSetup(t)
	main := filepath.Join(codex, "sessions", "2026", "09", "20", "rollout-2026-09-20T15-48-28-"+mgCX+".jsonl")
	original, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	db := codexStateDB(t, codex, mgCX, "openai")
	originalPrice := PriceOf
	changed := false
	PriceOf = func(current settings.Settings, model string) (catalog.Price, bool) {
		if !changed {
			changed = true
			mutationHistoryBinding(t, "codex")
		}
		return originalPrice(current, model)
	}
	t.Cleanup(func() { PriceOf = originalPrice })
	move, err := MoveCodexProvider(mgCX, "magpie")
	if !changed || !errors.Is(err, extension.ErrActionChanged) || move.Backup != "" {
		t.Errorf("move after delegated ownership: %+v, %v (changed %t)", move, err, changed)
	}
	after, err := os.ReadFile(main)
	if err != nil || !bytes.Equal(after, original) {
		t.Errorf("native rollout changed: %v", err)
	}
	provider, _, _ := threadProvider(t, db, mgCX)
	if provider != "openai" {
		t.Errorf("native DB provider changed: %s", provider)
	}
}
