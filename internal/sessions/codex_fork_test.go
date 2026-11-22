package sessions

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestCodexForkCoalescesParentReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parent.jsonl")
	writeLines(t, path, strings.TrimSpace(forkCheckpoint(1)))
	synctest.Test(t, func(t *testing.T) {
		var scans atomic.Int32
		release := make(chan struct{})
		previous := scanHook
		scanHook = func(name string, _ int64) {
			if name == path {
				scans.Add(1)
				<-release
			}
		}
		defer func() { scanHook = previous }()

		const readers = 5
		results := make(chan bool, readers)
		for range readers {
			go func() {
				marks, turns := codexCheckpointsOf(path)
				results <- len(marks) == 1 && len(turns) == 1 && turns["turn-1"]
			}()
		}
		synctest.Wait()
		close(release)
		for range readers {
			if !<-results {
				t.Error("the reader did not see the parent's checkpoint and turn")
			}
		}
		if count := scans.Load(); count != 1 {
			t.Fatalf("%d full parent reads for %d callers, want 1", count, readers)
		}
	})
}

func TestCodexForkReadsReplacementDuringParentScan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows prevents replacing a file opened by os.Open")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "parent.jsonl")
	replacement := filepath.Join(directory, "replacement.jsonl")
	writeLines(t, path, strings.TrimSpace(forkCheckpoint(1)))
	writeLines(t, replacement, strings.TrimSpace(forkCheckpoint(2)))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	synctest.Test(t, func(t *testing.T) {
		var scans atomic.Int32
		release := make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		previous := scanHook
		scanHook = func(name string, _ int64) {
			if name == path && scans.Add(1) == 1 {
				<-release
			}
		}
		defer func() { scanHook = previous }()
		defer unblock()

		before := make(chan bool, 1)
		go func() {
			marks, turns := codexCheckpointsOf(path)
			before <- len(marks) == 1 && len(turns) == 1 && turns["turn-1"]
		}()
		synctest.Wait()
		if scans.Load() != 1 {
			t.Fatal("the original parent scan did not reach the read")
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
		after := make(chan bool, 1)
		go func() {
			marks, turns := codexCheckpointsOf(path)
			after <- len(marks) == 1 && len(turns) == 1 && turns["turn-2"]
		}()
		synctest.Wait()
		select {
		case ok := <-after:
			if !ok {
				t.Error("the new reader did not get the replacement checkpoint and turn")
			}
		default:
			t.Error("the new reader waited on the prior parent")
		}
		unblock()
		if !<-before {
			t.Error("the original reader did not read the parent whose identity it saw")
		}
		synctest.Wait()
		marks, turns := codexCheckpointsOf(path)
		if len(marks) != 1 || len(turns) != 1 || !turns["turn-2"] || scans.Load() != 2 {
			t.Fatalf("the replacement did not stay cached, got %d marks, %v, %d scans", len(marks), turns, scans.Load())
		}
	})
}

func TestCodexForkCacheReplacesGrowingParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parent.jsonl")
	var history strings.Builder
	var firstMarks map[cxCheckpoint]bool
	var firstTurns map[string]bool
	for checkpoint := 1; checkpoint <= codexForksKept; checkpoint++ {
		history.WriteString(forkCheckpoint(checkpoint))
		if err := os.WriteFile(path, []byte(history.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		marks, turns := codexCheckpointsOf(path)
		if len(marks) != checkpoint || len(turns) != checkpoint {
			t.Fatalf("parent grew to %d, got %d checkpoints and %d turns", checkpoint, len(marks), len(turns))
		}
		if checkpoint == 1 {
			firstMarks, firstTurns = marks, turns
		}
	}
	if len(firstMarks) != 1 || len(firstTurns) != 1 || !firstTurns["turn-1"] {
		t.Fatalf("updating the cache changed a fork's in-use snapshot: %v, %v", firstMarks, firstTurns)
	}
	parents := 0
	codexForks.Lock()
	for _, entry := range codexForks.kept {
		if entry.path == path {
			parents++
		}
	}
	codexForks.Unlock()
	if parents != 1 {
		t.Fatalf("cache holds %d versions of one parent, want 1", parents)
	}
}

func TestCodexForkCacheTracksParentReplacement(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "parent.jsonl")
	if err := os.WriteFile(path, []byte(forkCheckpoint(1)), 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, turns := codexCheckpointsOf(path)
	if !turns["turn-1"] {
		t.Fatalf("the initial turn was absent: %v", turns)
	}
	replacement := filepath.Join(directory, "replacement.jsonl")
	if err := os.WriteFile(replacement, []byte(forkCheckpoint(2)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	marks, turns := codexCheckpointsOf(path)
	if len(marks) != 1 || len(turns) != 1 || !turns["turn-2"] {
		t.Fatalf("the replacement parent has stale checkpoints or turns: %v, %v", marks, turns)
	}
}

func forkCheckpoint(checkpoint int) string {
	return fmt.Sprintf(`{"type":"turn_context","payload":{"turn_id":"turn-%[1]d"}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%[1]d},"last_token_usage":{"input_tokens":1}}}}
`, checkpoint)
}

// A fork's rollout starts with every line of its parent's, token_count
// events included, stamped when the fork wrote them (codex-rs 0.117's
// InitialHistory::Forked). The fixtures are that shape: a gpt-5.4 parent of
// 2026-03-23 with three calls (and one more after the fork), and a
// spawn_agent fork of it on 03-24 that made one call of its own. The copies
// are the parent's calls, which the parent's rollout counts; read as the
// fork's, they put the parent's whole history on 03-24 again (Sky).
func TestCodexForkCountsOnlyItsOwnCalls(t *testing.T) {
	d := setupCalls(t)
	const parent, fork = "019d186b-a100-7aaa-8bbb-cccdddeeefff", "019d1f2d-f9c0-7111-8222-333444555666"
	place := func(fixture, day, stamp, id string) {
		b, err := os.ReadFile(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}
		writeLines(t, filepath.Join(d.codex, "sessions", "2026", "03", day, "rollout-2026-03-"+day+"T"+stamp+"-"+id+".jsonl"), strings.TrimSpace(string(b)))
	}
	place("codex-fork-parent.jsonl", "23", "10-00-00", parent)
	place("codex-fork-child.jsonl", "24", "17-30-00", fork)

	call := func(in, cached, out, reasoning int) Tokens {
		return Tokens{Input: in - cached, CacheRead: cached, Output: out}
	}
	own := map[string][]Tokens{
		parent: {call(41200, 3072, 812, 256), call(43680, 41088, 688, 144), call(46192, 44672, 900, 112), call(46928, 46080, 600, 88)},
		fork:   {call(47328, 44672, 550, 48)},
	}
	var want Tokens
	for _, ts := range own {
		for _, v := range ts {
			want.add(v)
		}
	}
	got := map[string]int{}
	var total Tokens
	var onFork Tokens
	for _, c := range Calls(time.Time{}) {
		got[c.File]++
		total.add(c.Tokens)
		if strings.Contains(c.File, fork) {
			onFork.add(c.Tokens)
		}
	}
	if total != want {
		t.Fatalf("calls booked %+v, want %+v (calls per file %v)", total, want, got)
	}
	if onFork != own[fork][0] {
		t.Fatalf("the fork's calls are %+v, want its own one call %+v", onFork, own[fork][0])
	}
	var listed Tokens
	for _, s := range List(0) {
		listed.add(s.Tokens)
	}
	if listed != want {
		t.Fatalf("sessions booked %+v, want %+v", listed, want)
	}
}

// With the parent's rollout gone, its calls are counted once, in the fork:
// they were made, and nothing else counts them now.
func TestCodexForkWithoutItsParentCountsTheCopies(t *testing.T) {
	d := setupCalls(t)
	b, err := os.ReadFile("testdata/codex-fork-child.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	writeLines(t, filepath.Join(d.codex, "sessions", "2026", "03", "24", "rollout-2026-03-24T17-30-00-019d1f2d-f9c0-7111-8222-333444555666.jsonl"), strings.TrimSpace(string(b)))
	if n := len(Calls(time.Time{})); n != 4 {
		t.Fatalf("%d calls, want the 3 copied and the fork's own", n)
	}
}

func TestUUIDV7Time(t *testing.T) {
	at, ok := uuidV7Time("019d186b-a100-7aaa-8bbb-cccdddeeefff")
	if !ok || !at.Equal(time.Date(2026, 3, 23, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("%v %v", at, ok)
	}
	if _, ok := uuidV7Time("4f2c1a7e-0000-4000-8000-000000000000"); ok {
		t.Fatal("a v4 id has no time")
	}
}

// A fork that went on working: its calls after the copies are its own,
// even one whose numbers are those of a call its parent made after the
// fork (both given the same next prompt over the same history). Only the
// copied lines at the head of the fork are the parent's (Sky,
// skyhuang1994: forks that went on working lost their own usage on
// ZCode; this is Codex's side of it).
func TestCodexForkThatWentOnWorkingKeepsItsOwnCalls(t *testing.T) {
	d := setupCalls(t)
	const parent, fork = "019d186b-a100-7aaa-8bbb-cccdddeeefff", "019d1f2d-f9c0-7111-8222-333444555666"
	read := func(name string) []string {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	p, c := read("codex-fork-parent.jsonl"), read("codex-fork-child.jsonl")
	// the fork's own first call: the parent's call after the fork, number
	// for number, at the fork's time; then a second call of its own
	same := strings.Replace(p[12], `"timestamp":"2026-03-24T10:00:20.000Z"`, `"timestamp":"2026-03-24T09:30:12.000Z"`, 1)
	if same == p[12] || !strings.Contains(c[14], `"type":"token_count"`) {
		t.Fatal("the fixtures changed")
	}
	c[14] = same
	next := strings.NewReplacer(
		`"timestamp":"2026-03-24T10:00:20.000Z"`, `"timestamp":"2026-03-24T09:40:00.000Z"`,
		`{"input_tokens":178000,"cached_input_tokens":134912,"output_tokens":3000,"reasoning_output_tokens":600,"total_tokens":181000}`,
		`{"input_tokens":228000,"cached_input_tokens":174912,"output_tokens":3400,"reasoning_output_tokens":650,"total_tokens":231400}`,
		`{"input_tokens":46928,"cached_input_tokens":46080,"output_tokens":600,"reasoning_output_tokens":88,"total_tokens":47528}`,
		`{"input_tokens":50000,"cached_input_tokens":40000,"output_tokens":400,"reasoning_output_tokens":50,"total_tokens":50400}`,
	).Replace(p[12])
	c = append(c, next)
	writeLines(t, filepath.Join(d.codex, "sessions", "2026", "03", "23", "rollout-2026-03-23T10-00-00-"+parent+".jsonl"), strings.Join(p, "\n"))
	writeLines(t, filepath.Join(d.codex, "sessions", "2026", "03", "24", "rollout-2026-03-24T17-30-00-"+fork+".jsonl"), strings.Join(c, "\n"))

	call := func(in, cached, out int) Tokens { return Tokens{Input: in - cached, CacheRead: cached, Output: out} }
	wantFork := call(46928, 46080, 600)
	wantFork.add(call(50000, 40000, 400))
	var onFork, all, want Tokens
	n := 0
	for _, x := range Calls(time.Time{}) {
		all.add(x.Tokens)
		if strings.Contains(x.File, fork) {
			onFork.add(x.Tokens)
			n++
		}
	}
	if n != 2 || onFork != wantFork {
		t.Fatalf("the fork's calls: %d %+v, want its 2 own %+v", n, onFork, wantFork)
	}
	for _, v := range []Tokens{call(41200, 3072, 812), call(43680, 41088, 688), call(46192, 44672, 900), call(46928, 46080, 600), wantFork} {
		want.add(v)
	}
	if all != want {
		t.Fatalf("calls booked %+v, want %+v", all, want)
	}
	var listed Tokens
	for _, s := range List(0) {
		listed.add(s.Tokens)
	}
	if listed != want {
		t.Fatalf("sessions booked %+v, want %+v", listed, want)
	}
}
