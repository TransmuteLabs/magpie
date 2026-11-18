package sessions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCacheSnapshotPersistsOwnedCodexWindow(t *testing.T) {
	setup(t)
	Saved()
	path := filepath.Join(t.TempDir(), "rollout-owned.jsonl")
	value := &state{
		Agent: "codex", Size: 5, Off: 5, ContentHash: digest{1},
		Codex: &codexUsageState{Models: map[string]string{"turn": "before"}},
	}
	snapshot := snapshotCacheState(value)
	value.Codex.Models["turn"] = "after"
	writeCodexWindows(map[string]*state{path: snapshot})
	if _, err := os.Stat(codexWindowPath(path)); err != nil {
		t.Fatalf("queued cache snapshot lost its Codex window: %v", err)
	}
	reloaded := *snapshot
	reloaded.Codex = nil
	restored := codexWindowed(file{agent: "codex", path: path}, &reloaded)
	if restored.Codex == nil || restored.Codex.Models["turn"] != "before" {
		t.Fatal("persisted Codex window borrowed changes made after the snapshot")
	}
}

func TestCacheSaveKeepsUnchangedCodexWindow(t *testing.T) {
	setup(t)
	Saved()
	path := filepath.Join(t.TempDir(), "rollout-unchanged.jsonl")
	value := &state{
		Agent: "codex", Size: 5, Off: 5, ContentHash: digest{2},
		Codex: &codexUsageState{Models: map[string]string{"turn": "first"}},
	}
	publish := func(current *state) {
		t.Helper()
		mu.Lock()
		cache = map[string]*state{path: current}
		saveCache()
		mu.Unlock()
		Saved()
	}
	publish(value)
	first, err := os.Stat(codexWindowPath(path))
	if err != nil {
		t.Fatal(err)
	}
	publish(value)
	second, err := os.Stat(codexWindowPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(first, second) {
		t.Fatal("saving unchanged state rewrote the Codex window")
	}
	next := value.clone()
	next.Codex.Models["turn"] = "second"
	publish(next)
	last, err := os.Stat(codexWindowPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(second, last) {
		t.Fatal("saving a new parsed state kept the old Codex window")
	}
	reloaded := *next
	reloaded.Codex = nil
	restored := codexWindowed(file{agent: "codex", path: path}, &reloaded)
	if restored.Codex == nil || restored.Codex.Models["turn"] != "second" {
		t.Fatal("new parsed Codex window was not persisted")
	}
}
