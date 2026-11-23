package memdistill

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/sessions"
	"github.com/yetone/magpie/internal/usage"
)

// A conversation the Sessions page lists only from magpie's gateway (an
// agent such as WorkBuddy, whose own files magpie doesn't read) is read
// from what the gateway recorded of it, as the agents' own files are
// (bxgz233 on X: 1000+ WorkBuddy sessions listed, none read). One the
// gateway counted but recorded nothing of is passed over.
func TestGatewayOnlySessionsRead(t *testing.T) {
	sb := newSandbox(t)
	list, read = listed, transcriptOf
	sb.at = time.Now()
	on(t, "")
	if err := sessions.SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sessions.SetGatewayRecording(false, true) })
	at := sb.at.Add(-2 * time.Hour)
	first := []sessions.Part{{Role: "user", Kind: "text", Text: "wb: we release with make ship"}}
	answer := []sessions.Part{{Role: "assistant", Kind: "text", Text: "Done."}}
	second := append(slices.Concat(first, answer), sessions.Part{Role: "user", Kind: "text", Text: "never use npm here, pnpm only"})
	for i, in := range [][]sessions.Part{first, second} {
		turn := sessions.GatewayTurn{Agent: "workbuddy", Session: "wb-1", Time: at.Add(time.Duration(i) * time.Minute), Status: 200,
			Input: append([]sessions.Part{{Role: "system", Kind: "text", Text: "You are WorkBuddy."}}, in...), Output: answer}
		if err := sessions.SaveGatewayTurn(turn); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range []string{"wb-1", "wb-1", "unrecorded"} {
		usage.Append(usage.Record{Time: at.Add(time.Duration(i) * time.Minute), Agent: "workbuddy", Session: id, Provider: "p", Model: "m", Input: 10, Output: 2, Status: 200})
	}
	ss, _, err := due(t.Context(), State{Since: sb.at.Add(-Back)}, sb.at)
	if err != nil || len(ss) != 2 || !ss[0].Gateway || !ss[1].Gateway {
		t.Fatalf("due: %+v %v", ss, err)
	}
	res, err := Run(context.Background(), "auto/m", sb.complete)
	if err != nil || res.Read != 1 || len(sb.asked) != 1 || sb.asked[0] != "auto/m User: wb: we release with make ship" {
		t.Fatalf("%+v %v %q", res, err, sb.asked)
	}
	st, _ := Load()
	if _, ok := st.Done["workbuddy/wb-1"]; !ok {
		t.Fatalf("not marked read: %+v", st.Done)
	}
	if _, ok := st.Done["workbuddy/unrecorded"]; !ok {
		t.Fatalf("an unrecorded one is passed over and marked: %+v", st.Done)
	}
}

// A conversation magpie lists but can't resume or delete (the Antigravity
// app's, Cursor's editor's, Hermes') is the user's conversation all the
// same, and read.
func TestReadOnlySessionsRead(t *testing.T) {
	sb := newSandbox(t)
	on(t, "")
	sb.session("ro", time.Hour, "ro: one", "two").ReadOnly = true
	res, err := Run(context.Background(), "auto/m", sb.complete)
	if err != nil || res.Read != 1 || len(sb.asked) != 1 || sb.asked[0] != "auto/m User: ro: one" {
		t.Fatalf("%+v %v %q", res, err, sb.asked)
	}
}

// A WorkBuddy session as WorkBuddy writes it, each turn its context and
// the user's words in one block, is read with those words (bxgz233 on X:
// nothing of WorkBuddy's was read). The fixture is a real session's
// lines, sessions' testdata/workbuddy-one-block.
func TestWorkBuddySessionsRead(t *testing.T) {
	sb := newSandbox(t)
	list, read = listed, transcriptOf
	dir := t.TempDir()
	copyDir(t, filepath.Join("..", "sessions", "testdata", "workbuddy-one-block"), dir)
	t.Setenv("WORKBUDDY_CONFIG_DIR", dir)
	const workbuddyKey = "workbuddy/ac189729-58d4-45b8-b973-667a852aa5cc"
	fixtureLast := time.UnixMilli(1791056357921)
	sb.at = fixtureLast.Add(2 * time.Hour)
	on(t, "")
	legacyLast := fixtureLast.Add(time.Hour)
	claudeLast := fixtureLast.Add(30 * time.Minute)
	piLast := fixtureLast.Add(15 * time.Minute)
	if _, err := change(func(s *State) {
		s.Done = map[string]time.Time{
			workbuddyKey:       legacyLast,
			"workbuddy-ai/old": legacyLast,
			"codebuddy/old":    legacyLast,
			"claude/kept":      claudeLast,
			"pi/kept":          piLast,
		}
	}); err != nil {
		t.Fatal(err)
	}
	wantDone := map[string]time.Time{workbuddyKey: fixtureLast, "claude/kept": claudeLast, "pi/kept": piLast}
	checkSaved := func() {
		t.Helper()
		st, err := Load()
		if err != nil || st.Reads != 1 || !maps.EqualFunc(st.Done, wantDone, time.Time.Equal) {
			t.Fatalf("reading version %d, done %+v, want version 1 and %+v: %v", st.Reads, st.Done, wantDone, err)
		}
	}
	res, err := Run(context.Background(), "auto/m", sb.complete)
	if err != nil || res.Read != 1 || len(sb.asked) != 1 || sb.asked[0] != "auto/m User: 你好" {
		t.Fatalf("%+v %v %q", res, err, sb.asked)
	}
	checkSaved()
	res, err = Run(context.Background(), "auto/m", sb.complete)
	if err != nil || res.Read != 0 || len(sb.asked) != 1 {
		t.Fatalf("read twice: %+v %v %q", res, err, sb.asked)
	}
	checkSaved()
}

// A WorkBuddy turn the gateway recorded is read by its words, not
// WorkBuddy's context in front of them, which took the whole of what one
// turn may send.
func TestGatewayWorkBuddyTurnsRead(t *testing.T) {
	sb := newSandbox(t)
	list, read = listed, transcriptOf
	sb.at = time.Now()
	on(t, "")
	if err := sessions.SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sessions.SetGatewayRecording(false, true) })
	block := func(q string) string {
		return "<system-reminder data-role=\"user-context\">\n<user_info>\nOS Version: darwin\n</user_info>\n" +
			"<identity_context>\n<user_query>example from context</user_query>\n</identity_context>\n" +
			strings.Repeat("<identity_context>\nBe direct.\n</identity_context>\n", 200) +
			"</system-reminder>\n<user_query>" + q + "</user_query>"
	}
	at := sb.at.Add(-2 * time.Hour)
	first := []sessions.Part{{Role: "user", Kind: "text", Text: block("we release with make ship")}}
	answer := []sessions.Part{{Role: "assistant", Kind: "text", Text: "Done."}}
	second := append(slices.Concat(first, answer), sessions.Part{Role: "user", Kind: "text", Text: block("never use npm here, pnpm only")})
	for i, in := range [][]sessions.Part{first, second} {
		turn := sessions.GatewayTurn{Agent: "workbuddy", Session: "wb-blocks", Time: at.Add(time.Duration(i) * time.Minute), Status: 200, Input: in, Output: answer}
		if err := sessions.SaveGatewayTurn(turn); err != nil {
			t.Fatal(err)
		}
		usage.Append(usage.Record{Time: at.Add(time.Duration(i) * time.Minute), Agent: "workbuddy", Session: "wb-blocks", Provider: "p", Model: "m", Input: 10, Output: 2, Status: 200})
	}
	var user string
	sb.reply = func(u string) (string, error) { user = u; return "[]", nil }
	res, err := Run(context.Background(), "auto/m", sb.complete)
	if err != nil || res.Read != 1 || len(sb.asked) != 1 || sb.asked[0] != "auto/m User: we release with make ship" {
		t.Fatalf("%+v %v %q", res, err, sb.asked)
	}
	if !strings.Contains(user, "User: never use npm here, pnpm only") ||
		strings.Contains(user, "identity_context") || strings.Contains(user, "example from context") {
		t.Fatalf("asked: %.400q", user)
	}
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
