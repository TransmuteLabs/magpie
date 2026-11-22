package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// Who answered a conversation is written to affinity.json behind the
// answer, once in a while: each answer marshalled up to 512 conversations
// and wrote and renamed the file, one answer after another.
func TestSticksWrittenBehind(t *testing.T) {
	fresh(t)
	was := sticksFlushAfter
	sticksFlushAfter = 2 * time.Second // as outside a test binary
	t.Cleanup(func() { flushSticks(); sticksFlushAfter = was })
	sticks.Lock()
	sticks.m = map[string]stick{}
	sticks.Unlock()
	os.Remove(sticksPath())
	c := candidate{p: provider.Provider{ID: "a", Key: "k"}, model: "m", rest: "a"}
	for i := range 200 {
		answered(fmt.Sprintf("conv-%d", i), c, 1, Usage{})
	}
	if _, err := os.Stat(sticksPath()); err == nil {
		t.Fatal("affinity.json written on a request answered")
	}
	sticks.Lock()
	_, ok := stickOf("conv-199")
	sticks.Unlock()
	if !ok {
		t.Fatal("the last answer isn't known in memory")
	}
	flushSticks() // as the gateway ends
	b, err := os.ReadFile(sticksPath())
	if err != nil {
		t.Fatalf("not written as the gateway ends: %v", err)
	}
	var saved map[string]savedStick
	if err := json.Unmarshal(b, &saved); err != nil || len(saved) != 200 {
		t.Fatalf("written %d conversations (%v), want 200", len(saved), err)
	}

	// and by itself, a moment later
	sticksFlushAfter = 10 * time.Millisecond
	os.Remove(sticksPath())
	answered("conv-late", c, 1, Usage{})
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if b, err := os.ReadFile(sticksPath()); err == nil && json.Unmarshal(b, &saved) == nil && saved["conv-late"].Who != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("an answer never written")
		}
	}
}

func BenchmarkAnswered(b *testing.B) {
	was := sticksFlushAfter
	sticksFlushAfter = 2 * time.Second // as outside a test binary
	b.Cleanup(func() { flushSticks(); sticksFlushAfter = was })
	sticks.Lock()
	sticks.m = map[string]stick{}
	for i := range sticksKept {
		sticks.m[fmt.Sprintf("conv-%d", i)] = stick{rest: "a", who: "a#k", model: "m", turn: 3, at: time.Now()}
	}
	sticks.Unlock()
	c := candidate{p: provider.Provider{ID: "a", Key: "k"}, model: "m", rest: "a"}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		answered(fmt.Sprintf("conv-%d", i%sticksKept), c, 3, Usage{Input: 100})
	}
	b.StopTimer()
	flushSticks()
}

func fillAffinitySnapshot(t testing.TB) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	now := time.Now().UTC()
	sticks.Lock()
	previous, previousFrom, previousMod := sticks.m, sticks.from, sticks.mod
	sticks.m = make(map[string]stick, sticksKept)
	for i := range sticksKept {
		at := now.Add(-time.Duration(i) * time.Second)
		sticks.m[fmt.Sprintf("conversation-%04d", i)] = stick{
			rest: "provider", who: "provider#account", model: "model", effort: "high",
			turn: 8, at: at, cacheRead: 1100, cacheWrite: 2200,
			hour: hourCache{tokens: 3300, at: at}, stoodIn: i%4 == 0,
		}
	}
	sticks.Unlock()
	t.Cleanup(func() {
		sticks.Lock()
		sticks.m, sticks.from, sticks.mod = previous, previousFrom, previousMod
		sticks.Unlock()
	})
}

func TestAffinitySnapshotAllocations(t *testing.T) {
	fillAffinitySnapshot(t)
	saveSticks()
	file, err := os.Stat(sticksPath())
	if err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(4, saveSticks)
	t.Logf("saved %d bytes, %.0f allocations per write", file.Size(), allocs)
	if allocs > 2*sticksKept {
		t.Errorf("saving %d sticks made %.0f allocations", sticksKept, allocs)
	}
}

func BenchmarkSaveSticks(b *testing.B) {
	fillAffinitySnapshot(b)
	b.ReportAllocs()
	for b.Loop() {
		saveSticks()
	}
	if _, err := os.Stat(sticksPath()); err != nil {
		b.Fatal(err)
	}
}
