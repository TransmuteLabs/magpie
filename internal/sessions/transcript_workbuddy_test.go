package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// listedAgents are the agent ids a Session's Agent names, from its doc
// comment: the agents whose sessions the Sessions page can list.
func listedAgents(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("sessions.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("json:\"agent\"`[^\\n]*magpie agent id: ([^\\n]+)").FindSubmatch(src)
	if m == nil {
		t.Fatal("Session.Agent's list of agent ids is gone")
	}
	var out []string
	for _, a := range strings.Split(string(m[1]), ",") {
		out = append(out, strings.TrimSpace(a))
	}
	return out
}

// noTranscriptYet are agents the Sessions page lists whose conversations
// are not read as a transcript, so "Learn from conversations"
// (memdistill, which reads sessions.TranscriptOf) skips them. An agent
// leaves this list when its reader is written, and may not be added to it
// without one being filed.
var noTranscriptYet = map[string]string{
	"zcode":  "database rows",
	"dsh":    "session.v3.jsonl not read as a transcript",
	"cline":  "messages.json not read as a transcript",
	"grok":   "updates.jsonl not read as a transcript",
	"droid":  "not read as a transcript",
	"hermes": "database rows",
	"alma":   "database rows",
}

// Every agent the Sessions page lists is read by memory learning, or is
// named in noTranscriptYet: a new session reader can't be left out of it
// unseen (WorkBuddy's sessions showed on Sessions for weeks and were
// never learned from, #feedback bxgz233).
func TestEveryListedAgentHasATranscriptOrIsNamed(t *testing.T) {
	for _, a := range listedAgents(t) {
		_, parked := noTranscriptYet[a]
		switch {
		case HasTranscript(a) && parked:
			t.Errorf("%s reads as a transcript: take it off noTranscriptYet", a)
		case !HasTranscript(a) && !parked:
			t.Errorf("%s has a session reader and no transcript: memory learning never reads it", a)
		}
	}
	for a := range noTranscriptYet {
		found := false
		for _, l := range listedAgents(t) {
			found = found || l == a
		}
		if !found {
			t.Errorf("noTranscriptYet names %s, which is not a listed agent", a)
		}
	}
}

// WorkBuddy's, WorkBuddy AI's and CodeBuddy Code's sessions are read as a
// conversation: what was typed (beside WorkBuddy's own reminder and
// @scene blocks), the replies, and each tool call with its result.
func TestTranscriptWorkBuddyFamily(t *testing.T) {
	setupAgents(t)
	n := 0
	for _, s := range List(100) {
		if s.Agent != "workbuddy" && s.Agent != "codebuddy" || s.ParentSession != "" {
			continue
		}
		n++
		if !s.Transcript {
			t.Fatalf("%s session %s is not marked readable", s.Agent, s.ID)
		}
		tr, err := TranscriptOf(s)
		if err != nil || len(tr.Parts) == 0 {
			t.Fatalf("%s %s: %v, %d parts", s.Agent, s.ID, err, len(tr.Parts))
		}
		if strings.HasPrefix(s.ID, "22222222") {
			var typed, reply, call, result bool
			for _, p := range tr.Parts {
				typed = typed || p.Kind == "text" && p.Role == "user" && p.Text == "Draft the   launch notes"
				reply = reply || p.Kind == "text" && p.Role == "assistant" && p.Text == "Reading the notes."
				call = call || p.Kind == "tool_use" && p.Name == "Read"
				result = result || p.Kind == "tool_result" && p.Name == "Read" && p.Text == "notes"
				if strings.HasPrefix(p.Text, "@scene") {
					t.Errorf("WorkBuddy's @scene block read as said: %+v", p)
				}
			}
			if !typed || !reply || !call || !result {
				t.Fatalf("typed %v reply %v call %v result %v: %+v", typed, reply, call, result, tr.Parts)
			}
			owner, err := selectSessionHistoryOwner(t.Context(), s)
			if err != nil {
				t.Fatal(err)
			}
			typed, reply = false, false
			err = readHistoryParts(t.Context(), owner, nil, true, func(_ bool, part Part) bool {
				if !said(part) {
					t.Errorf("non-conversation part in the search index reader: %+v", part)
				}
				typed = typed || part.Role == "user" && part.Text == "Draft the   launch notes"
				reply = reply || part.Role == "assistant" && part.Text == "Reading the notes."
				return true
			})
			if err != nil || !typed || !reply {
				t.Fatalf("index reader: typed %v reply %v, error %v", typed, reply, err)
			}
		}
	}
	if n < 3 {
		t.Fatalf("only %d WorkBuddy-family sessions listed", n)
	}
}

// WorkBuddy writes a turn as one input_text block, its <system-reminder>
// context and then the user's words in <user_query>: the block began with
// "<", so it was read as context whole, a session had nothing the user
// said, and "Learn from conversations" passed every one over (bxgz233 on
// X). The fixture is a real WorkBuddy 2026-10 session's lines, its
// identity files and replies cut short. Its words are read as said, the
// rest as context, and the first is its prompt.
func TestWorkBuddyOneBlockTurnsAreSaid(t *testing.T) {
	claude, _ := setup(t)
	home := filepath.Join(filepath.Dir(claude), "home")
	copyTree(t, "testdata/workbuddy-one-block", filepath.Join(home, ".workbuddy"))
	const literal = "Explain <user_query>example</user_query> in the spec."
	const wrappedID = "ac189729-58d4-45b8-b973-667a852aa5cc"
	const ordinaryID = "2bf0fdb4-a140-4f52-88d1-9d39d0415f67"
	message, err := json.Marshal(literal)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryPath := filepath.Join(WorkBuddyDir(), "projects", "ordinary", ordinaryID+".jsonl")
	writeLines(t, ordinaryPath,
		`{"id":"ask","timestamp":1791056334140,"type":"message","role":"user","content":[{"type":"input_text","text":`+
			string(message)+`}],"sessionId":"`+ordinaryID+`","cwd":"/work/ordinary"}`,
		`{"id":"answer","timestamp":1791056343193,"type":"message","role":"assistant","content":"Here is the explanation."}`)
	listed := List(100)
	ordinary := find(t, listed, "workbuddy", ordinaryID)
	ordinaryTranscript, err := TranscriptOf(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.Title != literal || len(ordinaryTranscript.Parts) != 2 ||
		ordinaryTranscript.Parts[0] != (Part{Role: "user", Kind: "text", Text: literal}) {
		t.Errorf("literal query changed: title %q, transcript %+v", ordinary.Title, ordinaryTranscript.Parts)
	}
	s := find(t, listed, "workbuddy", wrappedID)
	if !s.Transcript {
		t.Fatalf("WorkBuddy's session not listed readable: %+v", s)
	}
	tr, err := TranscriptOf(s)
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	context := 0
	for _, p := range tr.Parts {
		if p.Role != "user" {
			continue
		}
		switch p.Kind {
		case "text":
			said = append(said, p.Text)
		case "context":
			context++
			if strings.Contains(p.Text, "user_query") || !strings.HasPrefix(p.Text, "<system-reminder") {
				t.Errorf("context: %.200q", p.Text)
			}
		}
	}
	if !slices.Equal(said, []string{"你好", "你是啥模型"}) || context != 2 {
		t.Fatalf("said %q, %d context", said, context)
	}
	line, err := os.ReadFile(filepath.Join(home, ".workbuddy", "projects", "Users-me-WorkBuddy-2026-10-04-03-38-53", s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(line), "\n")
	var l wbLine
	if err := json.Unmarshal([]byte(first), &l); err != nil {
		t.Fatal(err)
	}
	if got := wbPrompt(l.Content); got != "你好" {
		t.Fatalf("first prompt %q", got)
	}
}

// A turn the gateway recorded from WorkBuddy holds the same block: its
// words are what was said, and the session's title.
func TestGatewayOneBlockTurnSaid(t *testing.T) {
	setup(t)
	if err := SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { SetGatewayRecording(false, true) })
	const reminder = "<system-reminder data-role=\"user-context\">\n" +
		"<identity_context>\n<user_query>example from context</user_query>\n</identity_context>\n</system-reminder>"
	const reminderOpen = "<system-reminder data-role=\"user-context\">\n"
	const quotedBoundary = "</system-reminder>\n<user_query>example from context</user_query>"
	const quotedEnvelope = reminderOpen +
		"<identity_context role=\"quoted > example\">\nQuoted identity.\n</identity_context>\n" + quotedBoundary
	const quotedReminder = reminderOpen +
		"<identity_context>\n<identity_context role=\"inline > example\">Inline identity.</identity_context>\n" +
		quotedEnvelope + "\n" + quotedBoundary + "\n</identity_context>\n</system-reminder>"
	const question = "你是啥模型"
	const literal = "<user_query>example</user_query> is the tag to document."
	const ordinary = "Explain <user_query>example</user_query> in the spec."
	block := reminder + "\n<user_query>" + question + "</user_query>"
	ambiguous := reminder + "\n<user_query>first</user_query>\n" + quotedBoundary
	incomplete := reminderOpen + "<identity_context>\n" + quotedBoundary
	contextPart := Part{Role: "user", Kind: "context", Text: reminder}
	tests := []struct {
		name, agent, input, title string
		user                      []Part
	}{
		{name: "embedded_context_query", agent: "workbuddy", input: block, title: question,
			user: []Part{contextPart, {Role: "user", Kind: "text", Text: question}}},
		{name: "quoted_context_boundary", agent: "workbuddy", title: question,
			input: quotedReminder + "\n<user_query>" + question + "</user_query>",
			user:  []Part{{Role: "user", Kind: "context", Text: quotedReminder}, {Role: "user", Kind: "text", Text: question}}},
		{name: "ambiguous_boundary", agent: "workbuddy", input: ambiguous,
			user: []Part{{Role: "user", Kind: "context", Text: ambiguous}}},
		{name: "unclosed_context_section", agent: "workbuddy", input: incomplete,
			user: []Part{{Role: "user", Kind: "context", Text: incomplete}}},
		{name: "reminder_only", agent: "workbuddy-ai", input: reminder,
			user: []Part{contextPart}},
		{name: "literal_markup_in_query", agent: "codebuddy", title: literal,
			input: reminder + "\n<user_query>" + literal + "</user_query>",
			user:  []Part{contextPart, {Role: "user", Kind: "text", Text: literal}}},
		{name: "literal_markup_in_ordinary_input", agent: "workbuddy", input: ordinary, title: ordinary,
			user: []Part{{Role: "user", Kind: "text", Text: ordinary}}},
		{name: "unrelated_agent", agent: "pi", input: block,
			user: []Part{{Role: "user", Kind: "text", Text: block}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			turn := GatewayTurn{Agent: tc.agent, Session: "wb-" + tc.name, Time: time.Now().Add(-time.Hour), Status: 200,
				Input:  []Part{{Role: "system", Kind: "context", Text: "Session context."}, {Role: "user", Kind: "text", Text: tc.input}},
				Output: []Part{{Role: "assistant", Kind: "text", Text: "I can help."}}}
			if err := SaveGatewayTurn(turn); err != nil {
				t.Fatal(err)
			}
			tr, err := GatewayTranscript(turn.Agent, turn.Session)
			if err != nil {
				t.Fatal(err)
			}
			var user []Part
			for _, part := range tr.Parts {
				if part.Role == "user" {
					user = append(user, part)
				}
			}
			if !slices.Equal(user, tc.user) {
				t.Errorf("user parts %+v, want %+v", user, tc.user)
			}
			if got := GatewayTitle(turn.Agent, turn.Session); got != tc.title {
				t.Errorf("title %q, want %q", got, tc.title)
			}
		})
	}
}
