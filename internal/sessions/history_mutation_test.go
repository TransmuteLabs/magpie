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
	"github.com/yetone/magpie/internal/filelock"
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

func TestDeleteRechecksNativeHistoryOwner(t *testing.T) {
	claude, _ := manageSetup(t)
	path := filepath.Join(claude, "projects", "-work-app", mgCC+".jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	originalPrice := PriceOf
	changed := false
	PriceOf = func(current settings.Settings, model string) (catalog.Price, bool) {
		if !changed {
			changed = true
			mutationHistoryBinding(t, "claude")
		}
		return originalPrice(current, model)
	}
	t.Cleanup(func() { PriceOf = originalPrice })
	trashed, err := Delete("claude", mgCC)
	if !changed || !errors.Is(err, extension.ErrActionChanged) || trashed.Key != "" {
		t.Errorf("delete after delegated ownership: %+v, %v (changed %t)", trashed, err, changed)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Errorf("native transcript changed: %v", err)
	}
	if trash := Trash(); len(trash) != 0 {
		t.Errorf("delete published a trash entry: %+v", trash)
	}
}

func TestRestoreKeepsTrashWhenHistoryIsDelegated(t *testing.T) {
	claude, _ := manageSetup(t)
	path := filepath.Join(claude, "projects", "-work-app", mgCC+".jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trashed, err := Delete("claude", mgCC)
	if err != nil {
		t.Fatal(err)
	}
	mutationHistoryBinding(t, "claude")
	if restored, err := Restore(trashed.Key); !errors.Is(err, extension.ErrActionChanged) || restored.Key != "" {
		t.Errorf("restore after delegated ownership: %+v, %v", restored, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("restore published native history: %v", err)
	}
	if trash := Trash(); len(trash) != 1 || trash[0].Key != trashed.Key {
		t.Fatalf("restore lost the trash entry: %+v", trash)
	}
	if err := os.Remove(sourceBindingsPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(trashed.Key); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Errorf("restored transcript differs: %v", err)
	}
}

func TestNativeTrashRefusesDuringHistoryAuthorityChange(t *testing.T) {
	for _, authority := range []func() string{sourceBindingsPath, storageConsentsPath} {
		claude, _ := manageSetup(t)
		path := filepath.Join(claude, "projects", "-work-app", mgCC+".jsonl")
		locked := func(operation func() (Trashed, error)) {
			t.Helper()
			unlock, held := filelock.Lock(authority()+".lock", 0)
			if !held {
				t.Fatal("could not hold history authority")
			}
			defer unlock()
			if trash, err := operation(); trash.Key != "" || !errors.Is(err, extension.ErrActionChanged) {
				t.Fatalf("native trash changed during an ownership update: %+v, %v", trash, err)
			}
		}
		locked(func() (Trashed, error) { return Delete("claude", mgCC) })
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
		trashed, err := Delete("claude", mgCC)
		if err != nil {
			t.Fatal(err)
		}
		locked(func() (Trashed, error) { return Restore(trashed.Key) })
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("transcript during restore refusal: %v", err)
		}
		if _, err := Restore(trashed.Key); err != nil {
			t.Fatal(err)
		}
	}
}
