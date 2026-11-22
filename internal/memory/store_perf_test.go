package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSearchUsePreservesHandwrittenFile(t *testing.T) {
	fresh(t)
	if err := os.Mkdir(Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	const original = "Deploys need the VPN\n"
	handWrite(t, mdOf("vpn"), original)
	for range 2 {
		hits, err := Search(Query{Text: "VPN"})
		if err != nil || len(hits) != 1 || hits[0].ID != "vpn" {
			t.Fatalf("search: %v, %v", hits, err)
		}
	}
	if got := readFile(t, mdOf("vpn")); got != original {
		t.Errorf("noting a use rewrote the handwritten memory: %q", got)
	}
	if got := get(t, "vpn"); got.Uses != 1 || got.Used.IsZero() {
		t.Fatalf("use not kept once: %+v", got)
	}
}

func TestMemoryIndexTracksFileReplacement(t *testing.T) {
	fresh(t)
	if err := os.Mkdir(Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	original := mdOf("deploy")
	replacement := filepath.Join(Dir(), "incoming")
	handWrite(t, original, "Deploys need the VPN\n")
	old := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(original, old, old); err != nil {
		t.Fatal(err)
	}
	if hits, err := Rank(Query{Text: "VPN"}); err != nil || len(hits) != 1 {
		t.Fatalf("initial rank: %v, %v", hits, err)
	}
	stamp, err := os.Stat(original)
	if err != nil {
		t.Fatal(err)
	}
	handWrite(t, replacement, "Deploys need the key\n")
	if err := os.Chtimes(replacement, stamp.ModTime(), stamp.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, original); err != nil {
		t.Fatal(err)
	}
	if hits, err := Rank(Query{Text: "key"}); err != nil || len(hits) != 1 || hits[0].Text != "Deploys need the key" {
		t.Fatalf("rank kept the previous inode's text: %v, %v", hits, err)
	}
	if err := os.WriteFile(original, []byte("Deploys need the PIN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(original, stamp.ModTime(), stamp.ModTime()); err != nil {
		t.Fatal(err)
	}
	if hits, err := Rank(Query{Text: "PIN"}); err != nil || len(hits) != 1 || hits[0].Text != "Deploys need the PIN" {
		t.Fatalf("rank kept the text from before the same-inode edit: %v, %v", hits, err)
	}
}

func TestMemoryIndexResultOwnsTagsAndSources(t *testing.T) {
	fresh(t)
	record := save(t, "Verify the indexed memory", Rule, Global, "fixture")
	hits, err := Rank(Query{Text: "indexed"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("rank: %v, %v", hits, err)
	}
	hits[0].Tags[0], hits[0].Sources[0].Agent = "changed", "changed"
	again := get(t, record.ID)
	if again.Tags[0] != "fixture" || again.Sources[0].Agent != "codex" {
		t.Fatalf("rank result aliases the index: %+v", again)
	}
}

func TestSearchUseRechecksCurrentRecords(t *testing.T) {
	at := fresh(t)
	removed := save(t, "VPN is mandatory", Fact, Global)
	kept := save(t, "VPN access expires after a day", Fact, Global)
	oldRanking := linkedSemantic.Ranker
	t.Cleanup(func() { linkedSemantic.Ranker = oldRanking })
	linkedSemantic.Ranker = sdk.MemorySemanticRanker{
		Ready: func() bool { return true },
		Rank: func(sdk.MemorySemanticRequest) map[string]float64 {
			if err := Remove(removed.ID); err != nil {
				t.Fatal(err)
			}
			if err := change(func(snapshot *file) error {
				current := find(snapshot, kept.ID)
				current.Uses, current.Used = 7, *at
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			return nil
		},
	}
	if hits, err := Search(Query{Text: "VPN"}); err != nil || len(hits) != 2 {
		t.Fatalf("search lost its original ranked view: %v, %v", hits, err)
	}
	var current meta
	if err := json.Unmarshal([]byte(readFile(t, in(metaName))), &current); err != nil {
		t.Fatal(err)
	}
	if _, exists := current.Used[removed.ID]; exists {
		t.Fatalf("noted a use for a record removed during ranking: %v", current.Used)
	}
	if got := current.Used[kept.ID]; got.Uses != 7 || !got.At.Equal(*at) {
		t.Fatalf("overwrote a use recorded during ranking: %v", got)
	}
}

func TestRankRequiresTheStoreLock(t *testing.T) {
	fresh(t)
	save(t, "Deploys need the VPN", Fact, Global)
	if err := os.Remove(Dir() + ".lock"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(Dir()+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	if hits, err := Rank(Query{Text: "VPN"}); err == nil || len(hits) != 0 {
		t.Fatalf("rank took a view without the store lock: %v, %v", hits, err)
	}
}

func BenchmarkMemorySearchStore(b *testing.B) {
	dir := b.TempDir()
	oldPath, oldNow := Path, now
	Path = func() string { return filepath.Join(dir, "memory.json") }
	at := time.Now()
	now = func() time.Time { return at }
	b.Cleanup(func() { Path, now = oldPath, oldNow })
	if err := os.Mkdir(Dir(), 0o700); err != nil {
		b.Fatal(err)
	}
	for position := range 192 {
		data := fmt.Sprintf("VPN note %03d. %s\n", position, strings.Repeat("Release validation and local routing. ", 12))
		if err := os.WriteFile(mdOf(fmt.Sprintf("fixture-%03d", position)), []byte(data), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	query := Query{Text: "VPN", Limit: 8}
	if hits, err := Search(query); err != nil || len(hits) != query.Limit {
		b.Fatalf("initial search: %v, %v", hits, err)
	}
	for _, scenario := range []string{"already-noted", "new-uses"} {
		b.Run(scenario, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if scenario == "new-uses" {
					at = at.Add(time.Minute + time.Second)
				}
				if hits, err := Search(query); err != nil || len(hits) != query.Limit {
					b.Fatalf("search: %v, %v", hits, err)
				}
			}
		})
	}
}
