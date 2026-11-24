package sessions

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"weak"
)

func TestForgetParsesReleasesSavedCodexWindow(t *testing.T) {
	const (
		parent      = "019d186b-a100-7aaa-8bbb-cccdddeeefff"
		fork        = "019d1f2d-f9c0-7111-8222-333444555666"
		checkpoints = 1024
	)
	dirs := setupCalls(t)
	dir := filepath.Join(dirs.codex, "sessions", "2026", "03", "23")
	parentPath := filepath.Join(dir, "rollout-2026-03-23T10-00-00-"+parent+".jsonl")
	forkPath := filepath.Join(dir, "rollout-2026-03-23T10-30-00-"+fork+".jsonl")
	start := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)
	checkpoint := func(number int) string {
		at := start.Add(time.Duration(number) * time.Second).Format(time.RFC3339)
		return fmt.Sprintf(`{"timestamp":%[1]q,"type":"turn_context","payload":{"turn_id":"turn-%[2]d","model":"fixture-model"}}
{"timestamp":%[1]q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%[3]d,"output_tokens":%[4]d},"last_token_usage":{"input_tokens":11,"output_tokens":3}}}}
`, at, number, number*11, number*3)
	}
	meta := func(id, parentID string) string {
		return fmt.Sprintf(`{"timestamp":"2026-03-23T10:00:00Z","type":"session_meta","payload":{"id":%q,"forked_from_id":%q,"model_provider":"fixture"}}
{"timestamp":"2026-03-23T10:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"synthetic window diagnostic"}}`, id, parentID)
	}
	var history strings.Builder
	for number := 1; number <= checkpoints; number++ {
		history.WriteString(checkpoint(number))
	}
	writeLines(t, parentPath, meta(parent, ""), strings.TrimSpace(history.String()))
	writeLines(t, forkPath, meta(fork, parent), strings.TrimSpace(history.String()))

	checkSessions := func(parentCalls, forkCalls int) {
		t.Helper()
		totals := map[string]Tokens{}
		for _, session := range List(0) {
			totals[session.ID] = session.Tokens
		}
		for id, calls := range map[string]int{parent: parentCalls, fork: forkCalls} {
			want := Tokens{Input: calls * 11, Output: calls * 3}
			if got, ok := totals[id]; !ok || got != want {
				t.Fatalf("session %s: present=%v tokens=%+v, want %+v", id, ok, got, want)
			}
		}
	}
	readWindow := func() (os.FileInfo, []byte) {
		t.Helper()
		path := codexWindowPath(forkPath)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return info, body
	}

	checkSessions(checkpoints, 0)
	Saved()
	firstInfo, firstBody := readWindow()
	checkSameWindow := func() {
		t.Helper()
		info, body := readWindow()
		if !os.SameFile(firstInfo, info) || !bytes.Equal(firstBody, body) {
			t.Fatal("unchanged Codex fork window was rewritten")
		}
	}

	appendText(t, parentPath, checkpoint(checkpoints+1))
	checkSessions(checkpoints+1, 0)
	Saved()
	checkSameWindow()

	origin := func() weak.Pointer[state] {
		mu.Lock()
		defer mu.Unlock()
		current := cache[forkPath]
		if current == nil || current.Codex == nil || len(current.Codex.Copied) != checkpoints {
			t.Fatal("fork parse lacks the copied checkpoint window")
		}
		return weak.Make(current)
	}()
	if !ForgetParses() {
		t.Fatal("session cache did not drop after save")
	}
	runtime.GC()
	runtime.GC()
	if origin.Value() != nil {
		t.Error("saved Codex fork state remains reachable after ForgetParses")
	}

	var parses atomic.Int32
	parseHook = func() { parses.Add(1) }
	t.Cleanup(func() { parseHook = nil })
	checkSessions(checkpoints+1, 0)
	if count := parses.Load(); count != 0 {
		t.Fatalf("reopening parsed %d journals after ForgetParses", count)
	}
	Saved()
	checkSameWindow()

	appendText(t, forkPath, checkpoint(checkpoints+2))
	checkSessions(checkpoints+1, 1)
	Saved()
	lastInfo, _ := readWindow()
	if os.SameFile(firstInfo, lastInfo) {
		t.Fatal("the fork's changed window was not published")
	}

	var calls []Call
	for _, source := range CallSources() {
		if source.Path == forkPath {
			calls = append(calls, ReadCallSource(source)...)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("fork Usage has %d calls, want only its own call", len(calls))
	}
	if call := calls[0]; call.Session != fork || call.Model != "fixture-model" || call.Tokens != (Tokens{Input: 11, Output: 3}) {
		t.Fatalf("fork Usage call = %+v, want the fork's own 11 input and 3 output tokens", call)
	}
}
