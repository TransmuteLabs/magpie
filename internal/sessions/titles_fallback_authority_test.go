package sessions

import (
	"fmt"
	"maps"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestTitlesMissingNonZCodeKeysSkipZCodeAuthority(t *testing.T) {
	dirs := consentHome(t)
	if err := SetReads(new(bool), map[string]bool{"claude": true}); err != nil {
		t.Fatal(err)
	}
	const id = "11111111-2222-4333-8444-555555555555"
	const key = "claude:" + id
	const prompt = "The title falls back to this prompt"
	writeLines(t, filepath.Join(dirs.claude, "projects", "-work-app", id+".jsonl"),
		fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":%q},"cwd":"/work/app","sessionId":%q}`, stamp(0), prompt, id))
	want := map[string]string{key: prompt}
	if got := Titles([]string{key}); !maps.Equal(got, want) {
		t.Fatalf("warm title map = %v, want %v", got, want)
	}

	var checks atomic.Int64
	off := func() bool {
		checks.Add(1)
		return false
	}
	previous := OwnOff.Swap(&off)
	t.Cleanup(func() { OwnOff.Store(previous) })
	read := func(missing int) int64 {
		t.Helper()
		keys := []string{key}
		for index := range missing {
			keys = append(keys, fmt.Sprintf("claude:absent-%d", index))
		}
		checks.Store(0)
		if got := Titles(keys); !maps.Equal(got, want) {
			t.Fatalf("title map with %d absent keys = %v, want %v", missing, got, want)
		}
		return checks.Load()
	}
	one, many, repeat := read(1), read(64), read(1)
	t.Logf("gateway-mode checks for 1/64/1 absent non-ZCode keys: %d/%d/%d", one, many, repeat)
	if one == 0 || one != repeat {
		t.Fatalf("single-key controls are inactive or inconsistent: %d/%d", one, repeat)
	}
	if many != one {
		t.Fatalf("63 more absent non-ZCode keys added %d history mode checks", many-one)
	}
}
