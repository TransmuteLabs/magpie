package sessions

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"
)

// Once nothing has read the parses for cacheIdle they are let go, and the
// next read has them back from sessions.json, reading no session file.
// They stay while a save is not on disk yet, or the last one failed.
func TestIdleParsesLetGoAndComeBackFromDisk(t *testing.T) {
	setup(t)
	before := List(0)
	if len(before) == 0 {
		t.Fatal("no sessions listed")
	}
	saved()
	idle := func() { cacheKept.last.Store(time.Now().Add(-2 * cacheIdle).UnixNano()) }

	saving.Lock()
	saving.failed = true
	saving.Unlock()
	idle()
	if dropIdleCache() || !loaded {
		t.Fatal("the parses were let go while the last save had failed")
	}
	saving.Lock()
	saving.failed = false
	saving.Unlock()

	cacheKept.last.Store(time.Now().UnixNano())
	if dropIdleCache() || !loaded {
		t.Fatal("the parses were let go though read just now")
	}

	memos := func() (identities, rollouts int) {
		codexIdentityMemo.Lock()
		identities = len(codexIdentityMemo.kept)
		codexIdentityMemo.Unlock()
		rolloutsSeen.Range(func(any, any) bool { rollouts++; return true })
		return
	}
	ccDirs := func() (n int) { ccListed.Range(func(any, any) bool { n++; return true }); return }
	if ccDirs() == 0 {
		t.Fatal("before: no Claude Code folder's listing kept; the test lists none")
	}
	// folders are listed so only on macOS (dirent.Stats)
	if i, r := memos(); i == 0 || r == 0 && runtime.GOOS == "darwin" {
		t.Fatalf("before: %d Codex identity lists and %d folders' rollouts kept; the test reads none", i, r)
	}

	idle()
	drops := cacheDrops.Load()
	if !dropIdleCache() || loaded || len(cache) != 0 || cacheDrops.Load() != drops+1 {
		t.Fatalf("idle parses kept: loaded %v, %d in memory", loaded, len(cache))
	}
	if i, r := memos(); i != 0 || r != 0 || ccDirs() != 0 {
		t.Fatalf("idle: %d Codex identity lists, %d folders' rollouts and %d Claude Code listings kept", i, r, ccDirs())
	}

	var parses atomic.Int32
	parseHook = func() { parses.Add(1) }
	t.Cleanup(func() { parseHook = nil })
	after := List(0)
	if n := parses.Load(); n != 0 {
		t.Fatalf("%d session files read again after the let-go; want the parses from disk", n)
	}
	if len(after) != len(before) {
		t.Fatalf("%d sessions after the let-go, %d before", len(after), len(before))
	}
	for i := range before {
		if after[i].ID != before[i].ID || after[i].Tokens != before[i].Tokens || after[i].Title != before[i].Title {
			t.Fatalf("session %d after the let-go = %+v, before %+v", i, after[i], before[i])
		}
	}
}

// A page gone from view lets the parses go at once (ForgetParses), read
// just now or not, and the next read has them back from sessions.json; a
// save not on disk yet still keeps them.
func TestForgetParsesLetsGoAtOnce(t *testing.T) {
	setup(t)
	before := List(0)
	if len(before) == 0 {
		t.Fatal("no sessions listed")
	}
	saved()
	cacheKept.last.Store(time.Now().UnixNano())

	saving.Lock()
	saving.failed = true
	saving.Unlock()
	if ForgetParses() || !loaded {
		t.Fatal("the parses were let go while the last save had failed")
	}
	saving.Lock()
	saving.failed = false
	saving.Unlock()

	if !ForgetParses() || loaded || len(cache) != 0 {
		t.Fatalf("parses read just now kept: loaded %v, %d in memory", loaded, len(cache))
	}
	if ForgetParses() {
		t.Fatal("ForgetParses said it let go with nothing loaded")
	}
	var parses atomic.Int32
	parseHook = func() { parses.Add(1) }
	t.Cleanup(func() { parseHook = nil })
	if after := List(0); len(after) != len(before) || parses.Load() != 0 {
		t.Fatalf("%d sessions after (%d before), %d files read again", len(after), len(before), parses.Load())
	}
}

func TestForgetParsesClearsCallCacheAndContinuations(t *testing.T) {
	dirs := setupCalls(t)
	now := time.Now().UTC().Truncate(time.Second)
	claudePath := filepath.Join(dirs.claude, "projects", "-proj", "idle-calls-cc.jsonl")
	codexID := "019d186b-a200-7aaa-8bbb-cccdddeee111"
	codexPath := filepath.Join(dirs.codex, "sessions", "2026", "03", "23", "rollout-2026-03-23T10-00-00-"+codexID+".jsonl")

	writeLines(t, claudePath,
		fmt.Sprintf(`{"type":"user","uuid":"u1","sessionId":"idle-calls-cc","timestamp":%q,"message":{"role":"user","content":"hi"}}`, now.Add(-2*time.Minute).Format(time.RFC3339)),
		fmt.Sprintf(`{"type":"assistant","uuid":"a1","sessionId":"idle-calls-cc","timestamp":%q,"message":{"id":"msg_1","role":"assistant","model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":20}}}`, now.Add(-time.Minute).Format(time.RFC3339)),
	)
	writeLines(t, codexPath,
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"model_provider":"fixture"}}`, now.Add(-2*time.Minute).Format(time.RFC3339), codexID),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"turn_id":"turn-1","model":"gpt-5.4"}}`, now.Add(-2*time.Minute).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":50,"output_tokens":10},"last_token_usage":{"input_tokens":50,"output_tokens":10}}}}`, now.Add(-time.Minute).Format(time.RFC3339)),
	)

	before := Calls(time.Time{})
	if len(before) != 2 {
		t.Fatalf("got %d calls, want 2", len(before))
	}

	var cachedRef weak.Pointer[callFile]
	callsMu.Lock()
	if entry := callCache[claudePath]; entry == nil {
		callsMu.Unlock()
		t.Fatal("expected claude callFile in callCache before ForgetParses")
	} else {
		cachedRef = weak.Make(entry)
	}
	callsMu.Unlock()

	if !ForgetParses() {
		t.Fatal("ForgetParses returned false while callCache was populated")
	}
	callsMu.Lock()
	cacheLen, orderLen := len(callCache), len(callOrder)
	contLen, contOrderLen := len(callContinuations), len(callContinuationOrder)
	callsMu.Unlock()
	if cacheLen != 0 || orderLen != 0 || contLen != 0 || contOrderLen != 0 {
		t.Fatalf("call caches retained after ForgetParses: cache=%d order=%d cont=%d contOrder=%d", cacheLen, orderLen, contLen, contOrderLen)
	}
	runtime.GC()
	runtime.GC()
	if cachedRef.Value() != nil {
		t.Fatal("callFile remained reachable after ForgetParses")
	}
	if ForgetParses() {
		t.Fatal("ForgetParses returned true when all caches were empty")
	}

	var scans atomic.Int32
	scanHook = func(string, int64) { scans.Add(1) }
	t.Cleanup(func() { scanHook = nil })
	after := Calls(time.Time{})
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("calls after ForgetParses differ:\ngot  %+v\nwant %+v", after, before)
	}
	if count := scans.Load(); count != 0 {
		t.Fatalf("reopening calls rescanned %d source files instead of loading shards", count)
	}
	scanHook = nil

	cacheKept.last.Store(time.Now().UnixNano())
	if dropIdleCache() {
		t.Fatal("dropIdleCache cleared callCache though read just now")
	}
	callsMu.Lock()
	keptWhileFresh := len(callCache)
	callsMu.Unlock()
	if keptWhileFresh == 0 {
		t.Fatal("callCache was cleared while fresh")
	}
	cacheKept.last.Store(time.Now().Add(-2 * cacheIdle).UnixNano())
	if !dropIdleCache() {
		t.Fatal("dropIdleCache did not clear stale callCache")
	}
	callsMu.Lock()
	keptAfterIdle := len(callCache)
	callsMu.Unlock()
	if keptAfterIdle != 0 {
		t.Fatalf("callCache retained %d entries after dropIdleCache", keptAfterIdle)
	}

	// Exercise ReadCallSource -> callContinuations -> ForgetParses -> incremental append.
	var codexSource CallSource
	for _, source := range CallSources() {
		if source.Path == codexPath {
			codexSource = source
		}
	}
	if codexSource.Path == "" {
		t.Fatal("missing codex call source")
	}
	if got := ReadCallSource(codexSource); len(got) != 1 {
		t.Fatalf("codex ReadCallSource = %d calls, want 1", len(got))
	}
	callsMu.Lock()
	contBefore := len(callContinuations)
	callsMu.Unlock()
	if contBefore == 0 {
		t.Fatal("expected callContinuations entry after ReadCallSource")
	}
	if !ForgetParses() {
		t.Fatal("ForgetParses returned false while callContinuations was populated")
	}
	callsMu.Lock()
	contAfter := len(callContinuations)
	callsMu.Unlock()
	if contAfter != 0 {
		t.Fatalf("callContinuations retained %d entries after ForgetParses", contAfter)
	}

	appendText(t, codexPath,
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"turn_id":"turn-2","model":"gpt-5.4"}}`+"\n"+
			`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":80,"output_tokens":25},"last_token_usage":{"input_tokens":30,"output_tokens":15}}}}`+"\n",
			now.Format(time.RFC3339), now.Add(time.Second).Format(time.RFC3339)),
	)
	var scannedFrom int64 = -1
	scanHook = func(path string, off int64) {
		if path == codexPath {
			scannedFrom = off
		}
	}
	for _, source := range CallSources() {
		if source.Path == codexPath {
			codexSource = source
		}
	}
	updated := ReadCallSource(codexSource)
	if len(updated) != 2 {
		t.Fatalf("updated codex calls = %d, want 2", len(updated))
	}
	if scannedFrom <= 0 {
		t.Fatalf("codex append rescanned from offset %d, want incremental offset > 0 from on-disk .rev", scannedFrom)
	}
}

func TestForgetParsesClearsCodexForksCache(t *testing.T) {
	const (
		parentID = "019d186b-a300-7aaa-8bbb-cccdddeee222"
		forkID   = "019d1f2d-f9c0-7111-8222-333444555222"
	)
	dirs := setupCalls(t)
	dir := filepath.Join(dirs.codex, "sessions", "2026", "03", "23")
	parentPath := filepath.Join(dir, "rollout-2026-03-23T10-00-00-"+parentID+".jsonl")
	forkPath := filepath.Join(dir, "rollout-2026-03-23T10-30-00-"+forkID+".jsonl")
	common := `{"timestamp":"2026-03-23T10:00:01Z","type":"turn_context","payload":{"turn_id":"turn-1","model":"fixture-model"}}` + "\n" +
		`{"timestamp":"2026-03-23T10:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":11,"output_tokens":3},"last_token_usage":{"input_tokens":11,"output_tokens":3}}}}`
	writeLines(t, parentPath,
		fmt.Sprintf(`{"timestamp":"2026-03-23T10:00:00Z","type":"session_meta","payload":{"id":%q,"model_provider":"fixture"}}`, parentID),
		`{"timestamp":"2026-03-23T10:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"parent prompt"}}`,
		common,
	)
	writeLines(t, forkPath,
		fmt.Sprintf(`{"timestamp":"2026-03-23T10:30:00Z","type":"session_meta","payload":{"id":%q,"forked_from_id":%q,"model_provider":"fixture"}}`, forkID, parentID),
		`{"timestamp":"2026-03-23T10:30:00Z","type":"event_msg","payload":{"type":"user_message","message":"fork prompt"}}`,
		common,
	)

	if sessions := List(0); len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}
	Saved()

	codexForks.Lock()
	beforeKept := len(codexForks.kept)
	codexForks.Unlock()
	if beforeKept == 0 {
		t.Fatal("expected codexForks.kept to hold parent checkpoints before ForgetParses")
	}

	if !ForgetParses() {
		t.Fatal("ForgetParses returned false")
	}
	codexForks.Lock()
	afterKept := len(codexForks.kept)
	codexForks.Unlock()
	if afterKept != 0 {
		t.Fatalf("codexForks.kept retained %d entries after ForgetParses", afterKept)
	}

	// Concurrent ForgetParses during an active parent scan must not break waiting
	// readers and must not repopulate codexForks.kept with a pre-drop generation.
	if len(List(0)) != 2 {
		t.Fatal("reloading sessions from disk failed")
	}
	Saved()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	scanHook = func(path string, _ int64) {
		if path == parentPath {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
	}
	t.Cleanup(func() { scanHook = nil })

	type result struct {
		marks int
		turns int
	}
	results := make(chan result, 2)
	readParent := func() {
		marks, turns := codexCheckpointsOf(parentPath)
		results <- result{len(marks), len(turns)}
	}
	go readParent()
	<-entered
	go readParent()
	for {
		codexForks.Lock()
		joined := len(codexForks.reading) == 1 && codexForks.reading[0].waiters == 1
		codexForks.Unlock()
		if joined {
			break
		}
		runtime.Gosched()
	}
	if !ForgetParses() {
		t.Fatal("ForgetParses returned false while sessions were loaded")
	}
	close(release)
	for range 2 {
		got := <-results
		if got.marks != 1 || got.turns != 1 {
			t.Fatalf("concurrent codexCheckpointsOf = %+v, want {marks:1 turns:1}", got)
		}
	}
	codexForks.Lock()
	afterConcurrent := len(codexForks.kept)
	codexForks.Unlock()
	if afterConcurrent != 0 {
		t.Fatalf("stale in-flight scan repopulated codexForks.kept (%d entries)", afterConcurrent)
	}

	scanHook = nil
	marks, turns := codexCheckpointsOf(parentPath)
	if len(marks) != 1 || len(turns) != 1 {
		t.Fatalf("repopulated codexCheckpointsOf = (%d marks, %d turns), want (1, 1)", len(marks), len(turns))
	}
	codexForks.Lock()
	repopulated := len(codexForks.kept)
	codexForks.Unlock()
	if repopulated != 1 {
		t.Fatalf("codexForks.kept = %d after repopulation, want 1", repopulated)
	}
}

func TestForgetParsesClearsCCUUIDMemoAndReloadsFromDisk(t *testing.T) {
	ccCompactedBranchFixture(t)
	if days, sessions := ccStatTokens(t); days.Input != 80 || sessions.Input != 80 {
		t.Fatalf("before ForgetParses: tokens in = %d (days), %d (sessions); want 80", days.Input, sessions.Input)
	}
	Saved()

	var uuidRef weak.Pointer[ccUUIDs]
	var restRef weak.Pointer[ccRestOf]
	var keeperPath string
	mu.Lock()
	if len(ccUUIDMemo) == 0 || len(ccRestMemo) == 0 || len(ccFamilies.groups) == 0 {
		mu.Unlock()
		t.Fatalf("before ForgetParses: ccUUIDMemo=%d ccRestMemo=%d ccFamilies=%d", len(ccUUIDMemo), len(ccRestMemo), len(ccFamilies.groups))
	}
	for path, entry := range ccUUIDMemo {
		if strings.HasSuffix(path, ccCompacted+".jsonl") {
			keeperPath = path
			uuidRef = weak.Make(entry)
		}
	}
	for _, entry := range ccRestMemo {
		restRef = weak.Make(entry)
		break
	}
	mu.Unlock()
	if keeperPath == "" || uuidRef.Value() == nil || restRef.Value() == nil {
		t.Fatal("failed to capture ccUUIDMemo and ccRestMemo weak probes")
	}

	if !ForgetParses() {
		t.Fatal("ForgetParses returned false")
	}
	mu.Lock()
	uuidsAfter, restAfter, famAfter := len(ccUUIDMemo), len(ccRestMemo), len(ccFamilies.groups)
	mu.Unlock()
	if uuidsAfter != 0 || restAfter != 0 || famAfter != 0 {
		t.Fatalf("Claude copy memos retained after ForgetParses: uuids=%d rest=%d families=%d", uuidsAfter, restAfter, famAfter)
	}
	runtime.GC()
	runtime.GC()
	if uuidRef.Value() != nil || restRef.Value() != nil {
		t.Fatalf("Claude copy memos remained reachable after ForgetParses: uuidLive=%t restLive=%t", uuidRef.Value() != nil, restRef.Value() != nil)
	}

	// Reopening unchanged files hits on-disk cc-families.json and cc-rest.json without rescanning uuids.
	if days, sessions := ccStatTokens(t); days.Input != 80 || sessions.Input != 80 {
		t.Fatalf("after ForgetParses: tokens in = %d (days), %d (sessions); want 80", days.Input, sessions.Input)
	}
	mu.Lock()
	reopenedUUIDs := len(ccUUIDMemo)
	mu.Unlock()
	if reopenedUUIDs != 0 {
		t.Fatalf("reopening unchanged Claude family scanned %d uuid files", reopenedUUIDs)
	}

	// Appending a new turn reloads the keeper's uuids from on-disk cc-uuids/*.bin incrementally.
	infoBefore, err := os.Stat(keeperPath)
	if err != nil {
		t.Fatal(err)
	}
	appendText(t, keeperPath,
		`{"type":"assistant","uuid":"99999999-9999-4999-8999-999999999999","sessionId":"`+ccCompacted+`","timestamp":"2026-03-23T12:00:00Z","message":{"id":"msg_idle_extra","role":"assistant","model":"claude-sonnet-4-6","usage":{"input_tokens":15,"output_tokens":5}}}`+"\n",
	)
	var scannedUUIDOff int64 = -1
	var fullRescan bool
	scanHook = func(path string, off int64) {
		if path == keeperPath {
			if off == 0 {
				fullRescan = true
			} else if off == infoBefore.Size() {
				scannedUUIDOff = off
			}
		}
	}
	t.Cleanup(func() { scanHook = nil })
	if days, sessions := ccStatTokens(t); days.Input != 95 || sessions.Input != 95 {
		t.Fatalf("after append: tokens in = %d (days), %d (sessions); want 95", days.Input, sessions.Input)
	}
	if fullRescan {
		t.Fatal("uuid reload rescanned keeper from offset 0 instead of using on-disk cc-uuids")
	}
	if scannedUUIDOff != infoBefore.Size() {
		t.Fatalf("uuid reload did not continue incrementally from %d (saw %d)", infoBefore.Size(), scannedUUIDOff)
	}
}
