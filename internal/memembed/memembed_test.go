package memembed_test

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/memembed"
	"github.com/yetone/magpie/internal/memory"
	"github.com/yetone/magpie/internal/pii/guard"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestMain(m *testing.M) { testenv.Main(m) }

// fake reads a text as the topics it names, in either language: what a
// real model does for a search across languages, without its 135 MB.
type fake struct{ calls atomic.Int32 }

var topics = [][]string{
	{"release", "ship", "上线", "发布"},
	{"tabs", "indent", "缩进"},
	{"port", "端口"},
}

func (f *fake) Embed(text string) []float32 {
	f.calls.Add(1)
	v := make([]float32, guard.EmbedDim)
	v[len(v)-1] = 0.5 // something in common, as real texts have
	for i, words := range topics {
		for _, w := range words {
			if strings.Contains(strings.ToLower(text), w) {
				v[i] = 1
			}
		}
	}
	var sq float64
	for _, x := range v {
		sq += float64(x) * float64(x)
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(sq))
	}
	return v
}

// sandbox points memembed and memory at a folder of the test's own, with
// the model's files on disk at their sizes (sparse) and a fake model.
func sandbox(t *testing.T, installed bool) *fake {
	t.Helper()
	d := t.TempDir()
	oldMem := memory.Path
	memory.Path = func() string { return filepath.Join(d, "memory.json") }
	t.Cleanup(func() { memory.Path = oldMem })
	memembed.ModelHomeForTest(t, d)
	f := &fake{}
	memembed.UseEmbedderForTest(f.Embed)
	if installed {
		memembed.SparseModelForTest(t)
		b := []byte(`{"on":true}`)
		if err := os.WriteFile(memembed.StatePath(), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func index(ctx context.Context) (int, error) {
	return memembed.IndexForTest(ctx, memory.SemanticIndexSnapshot)
}

func save(t *testing.T, text, status string) *memory.Memory {
	t.Helper()
	k, sc := memory.Fact, memory.Global
	m, _, err := memory.Save(memory.Input{Text: &text, Kind: &k, Scope: &sc, Status: &status}, memory.FromUser, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func ids(hs []memory.Hit) []string {
	out := []string{}
	for _, h := range hs {
		out = append(out, h.Text)
	}
	return out
}

// A search finds what means the same in other words and another language,
// beside what holds its words; switched off, by words alone.
func TestSearchFindsByMeaning(t *testing.T) {
	sandbox(t, true)
	save(t, "Releases go out on Tuesdays", memory.Active)
	save(t, "Use tabs, never spaces", memory.Active)
	save(t, "A busy port is someone else's", memory.Active)
	hits, err := memory.Search(memory.Query{Text: "什么时候上线"})
	if err != nil || len(hits) != 1 || hits[0].Text != "Releases go out on Tuesdays" {
		t.Fatalf("%v %v", ids(hits), err)
	}
	// found both ways ahead of found one way: "never" is in one, and
	// both mean 缩进
	save(t, "Indent Go files with tabs", memory.Active)
	hits, _ = memory.Search(memory.Query{Text: "never 缩进"})
	if got := ids(hits); len(got) != 2 || got[0] != "Use tabs, never spaces" || got[1] != "Indent Go files with tabs" {
		t.Fatalf("%v", got)
	}
	// one common word ranks as high as the nearest in meaning; the nearer
	// in meaning goes first, though the other is newer
	save(t, "Never delete images when freeing disk space", memory.Active)
	hits, _ = memory.Search(memory.Query{Text: "when do we release"})
	if got := ids(hits); len(got) != 2 || got[0] != "Releases go out on Tuesdays" {
		t.Fatalf("%v", got)
	}
	if err := memory.Semantic().Set(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(memembed.Root()); !os.IsNotExist(err) {
		t.Fatalf("the model is still there: %v", err)
	}
	if hits, _ := memory.Search(memory.Query{Text: "什么时候上线"}); len(hits) != 0 {
		t.Fatalf("off, by meaning: %v", ids(hits))
	}
}

// Index reads what a search can return and has no vector for its text:
// once, again when its text changes, and the old text's vector goes. The
// vectors are kept on disk, and a file cut short is read as none.
func TestIndexReadsOnce(t *testing.T) {
	f := sandbox(t, true)
	a := save(t, "Releases go out on Tuesdays", memory.Active)
	candidate := save(t, "Use tabs", memory.Candidate)
	save(t, "Old port rule", memory.Archived)
	if n, err := index(context.Background()); err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	if n, _ := index(context.Background()); n != 0 || f.calls.Load() != 2 {
		t.Fatalf("read again: %d, %d calls", n, f.calls.Load())
	}
	text := "Releases go out on Wednesdays"
	if _, err := memory.Edit(a.ID, memory.Input{Text: &text}, true); err != nil {
		t.Fatal(err)
	}
	if n, _ := index(context.Background()); n != 1 {
		t.Fatalf("edited: %d", n)
	}
	if st := memory.Semantic().Status(); st.Read != 2 || st.Of != 2 || !st.On || !st.Installed {
		t.Fatalf("%+v", st)
	}
	path := filepath.Join(memembed.CacheDirForTest(), "vectors.bin")
	if got := memembed.VectorCountForTest(path); got != 2 {
		t.Fatalf("on disk: %d", got)
	}
	if err := memory.Remove(candidate.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := index(t.Context()); n != 0 || err != nil {
		t.Fatalf("pruning indexed a passage: %d, %v", n, err)
	}
	if st, persisted := memory.Semantic().Status(), memembed.VectorCountForTest(path); st.Read != 1 || st.Of != 1 || persisted != 1 {
		t.Fatalf("removed candidate follows the index: %+v, %d on disk", st, persisted)
	}
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, b[:len(b)-1], 0o644); err != nil {
		t.Fatal(err)
	}
	if got := memembed.VectorCountForTest(path); got != 0 {
		t.Fatalf("cut short: %d", got)
	}
}

func TestUnknownMemoryRecordDoesNotPruneSemanticIndex(t *testing.T) {
	model := sandbox(t, true)
	item := save(t, "Known indexed passage", memory.Active)
	if count, err := index(t.Context()); err != nil || count != 1 {
		t.Fatalf("initial index: %d (%v)", count, err)
	}
	vectorsFile := filepath.Join(memembed.CacheDirForTest(), "vectors.bin")
	previous, err := os.ReadFile(vectorsFile)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(memory.Dir(), item.ID+".md")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nsomething: [\n---\nBroken Memory fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if count, err := index(t.Context()); err == nil || count != 0 {
		t.Errorf("index treated an unreadable record as absent: %d (%v)", count, err)
	}
	if body, err := os.ReadFile(vectorsFile); err != nil || string(body) != string(previous) {
		t.Fatalf("unknown record changed the durable index: %v", err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if count, err := index(t.Context()); err != nil || count != 0 || model.calls.Load() != 1 {
		t.Fatalf("restored record was indexed again: %d (%v), model calls %d", count, err, model.calls.Load())
	}
	if status := memory.Semantic().Status(); status.Of != 1 || status.Read != 1 {
		t.Fatalf("restored index not reported: %+v", status)
	}
}

func TestIndexCannotPublishAfterSwitchOff(t *testing.T) {
	for _, mode := range []string{"off", "re-enabled"} {
		t.Run(mode, func(t *testing.T) {
			model := sandbox(t, true)
			item := save(t, "Releases go out on Tuesdays", memory.Active)
			entered, unblock := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(unblock) })
			memembed.UseEmbedderForTest(func(text string) []float32 {
				close(entered)
				<-unblock
				return model.Embed(text)
			})
			done := make(chan struct{})
			var indexed int
			var indexErr error
			t.Cleanup(func() { release(); <-done })
			go func() {
				defer close(done)
				indexed, indexErr = index(t.Context())
			}()
			select {
			case <-entered:
			case <-time.After(12 * time.Second):
				t.Fatal("index never started embedding")
			}
			if err := memory.Semantic().Set(false); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(memembed.Root()); !os.IsNotExist(err) {
				t.Fatalf("model directory remains after switching off: %v", err)
			}
			if mode == "re-enabled" {
				memembed.SparseModelForTest(t)
				if err := memory.Semantic().Set(true); err != nil {
					t.Fatal(err)
				}
			}
			status := make(chan sdk.MemorySemanticStatus, 1)
			go func() { status <- memory.Semantic().Status() }()
			select {
			case current := <-status:
				on := mode == "re-enabled"
				if current.On != on || current.Installed != on || current.Read != 0 {
					t.Errorf("status after switching: %+v", current)
				}
			case <-time.After(4 * time.Second):
				t.Error("status after switching waited for the previous embedding")
				release()
				<-status
			}
			release()
			<-done
			if _, err := os.Stat(filepath.Join(memembed.CacheDirForTest(), "vectors.bin")); !os.IsNotExist(err) {
				t.Fatalf("old indexing run published after switching off: %v", err)
			}
			if indexErr != nil || indexed != 0 {
				t.Fatalf("revoked index reported %d reads, %v", indexed, indexErr)
			}
			memembed.UseEmbedderForTest(model.Embed)
			expected := 0
			if mode == "re-enabled" {
				expected = 1
			}
			if count, err := index(t.Context()); err != nil || count != expected {
				t.Fatalf("new index reported %d reads, %v; want %d reads", count, err, expected)
			}
			if got, err := memory.Get(item.ID); err != nil || got == nil || got.Text != item.Text {
				t.Fatalf("switching off indexing changed Memory: %+v, %v", got, err)
			}
		})
	}
}

func TestIndexUsesTheCurrentCacheDirectory(t *testing.T) {
	sandbox(t, true)
	save(t, "Releases go out on Tuesdays", memory.Active)
	if count, err := index(t.Context()); err != nil || count != 1 {
		t.Fatalf("initial index: %d, %v", count, err)
	}
	if err := os.Rename(memembed.Root(), memembed.Root()+"-previous"); err != nil {
		t.Fatal(err)
	}
	memembed.SparseModelForTest(t)
	if st := memory.Semantic().Status(); st.Read != 0 || st.Of != 1 {
		t.Fatalf("replaced directory kept previous vector state: %+v", st)
	}
	if count, err := index(t.Context()); err != nil || count != 1 {
		t.Fatalf("replacement index: %d, %v", count, err)
	}
	if got := memembed.VectorCountForTest(filepath.Join(memembed.CacheDirForTest(), "vectors.bin")); got != 1 {
		t.Fatalf("replacement index on disk: %d", got)
	}
}

func TestLinkedMemoryKeepsTheSemanticIndex(t *testing.T) {
	sandbox(t, true)
	save(t, "Releases go out on Tuesdays", memory.Active)
	save(t, "An archived port rule", memory.Archived)
	semantic := memory.Semantic()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		semantic.Keep(ctx, time.Hour)
	}()
	t.Cleanup(func() { cancel(); <-done })

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	path := filepath.Join(memembed.CacheDirForTest(), "vectors.bin")
	for memembed.VectorCountForTest(path) != 1 {
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatalf("linked keeper did not index Memory: %+v", semantic.Status())
		}
	}
	if status := semantic.Status(); status.Of != 1 || status.Read != 1 {
		t.Fatalf("linked keeper and status read different snapshots: %+v", status)
	}
}

// With the model not on disk, or a switch file that can't be read, a
// search is by words and nothing is loaded.
func TestNotThereIsWords(t *testing.T) {
	f := sandbox(t, false)
	save(t, "Releases go out on Tuesdays", memory.Active)
	if hits, _ := memory.Search(memory.Query{Text: "上线"}); len(hits) != 0 || f.calls.Load() != 0 {
		t.Fatalf("%v, %d calls", ids(hits), f.calls.Load())
	}
	if err := os.WriteFile(memembed.StatePath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if memory.Semantic().On() {
		t.Fatal("a broken switch file is on")
	}
}
