package sessions

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/codexsummary"
)

// The fixtures are real sessions of the same project, made in a sandbox
// home: Claude Code 2.1.310 running Bash, Write, Read and Edit over two
// turns, and Codex 0.162.1 running exec_command (apply_patch among them)
// over two. Only the attachments' text (the system prompt and instruction
// files Claude Code attached) is taken out; their lines stay, as the chain
// of parents runs through them.
const (
	carryCwd    = "/Users/yetone/workspace/projects/magpie-wt/carry-sb/proj"
	carryClaude = "5b0c7a1e-3f2d-4c8b-9a6e-1d2c3b4a5f60"
	carryCodex  = "01a1712c-4d18-79a3-8dad-145c6b40351e"
)

func carrySetup(t *testing.T) (claudeSrc, codexSrc string) {
	claude, codex := setup(t)
	name, err := claudeProjectName(carryCwd)
	if err != nil {
		t.Fatal(err)
	}
	claudeSrc = filepath.Join(claude, "projects", name, carryClaude+".jsonl")
	codexSrc = filepath.Join(codex, "sessions", "2026", "10", "25", "rollout-2026-10-25T11-48-11-"+carryCodex+".jsonl")
	for src, to := range map[string]string{"testdata/carry/claude-tools.jsonl": claudeSrc, "testdata/carry/codex-tools.jsonl": codexSrc} {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Dir(to), 0o755)
		if err := os.WriteFile(to, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(codex, "config.toml"), []byte("model_provider = \"magpie\"\n"), 0o644)
	old := agentThere
	agentThere = func(string) bool { return true }
	t.Cleanup(func() {
		agentThere = old
		carriedMu.Lock()
		carried = map[carryKey]CarryResult{}
		carriedMu.Unlock()
	})
	Reset()
	return
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func sum(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A Claude Code session is carried on in Codex: a new rollout in Codex's
// own folder, which Codex's own reader takes as a rollout (its provider the
// one Codex uses now, no base_instructions of magpie's), the Bash call as
// Codex's exec_command with its output, the other tools as what the agent
// said, the thinking left behind, and the Claude Code file as it was.
func TestCarryClaudeToCodex(t *testing.T) {
	src, _ := carrySetup(t)
	before := sum(t, src)
	s, ok := Find("claude", carryClaude)
	if !ok {
		t.Fatal("claude session not found")
	}
	if want := []Carry{{Agent: "codex", Convert: true}, {Agent: "omp", Convert: true}}; !reflect.DeepEqual(s.Carry, want) {
		t.Fatalf("carry %+v", s.Carry)
	}
	r, err := CarryOver(s, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if r.Agent != "codex" || !strings.HasPrefix(r.Path, filepath.Join(CodexDir(), "sessions")+string(filepath.Separator)) ||
		!strings.HasSuffix(r.Path, "-"+r.ID+".jsonl") || !strings.Contains(r.Command, "codex resume "+r.ID) {
		t.Fatalf("result %+v", r)
	}
	if sum(t, src) != before {
		t.Fatal("the source was changed")
	}
	raw := sum(t, r.Path)
	lines := readLines(t, r.Path)
	meta := lines[0]["payload"].(map[string]any)
	if lines[0]["type"] != "session_meta" || meta["id"] != r.ID || meta["cwd"] != carryCwd || meta["model_provider"] != "magpie" || meta["base_instructions"] != nil {
		t.Fatalf("session_meta %v", lines[0])
	}
	if !strings.HasPrefix(raw, `{"timestamp":`) {
		t.Fatalf("a line's fields not in Codex's order: %.80s", raw)
	}
	for _, no := range []string{"The user wants me to", "thinking", "signature", "reasoning"} {
		if strings.Contains(raw, no) {
			t.Fatalf("%q carried over", no)
		}
	}
	var users, says []string
	var call, out map[string]any
	for _, l := range lines[1:] {
		p := l["payload"].(map[string]any)
		switch {
		case l["type"] == "event_msg" && p["type"] == "user_message":
			users = append(users, p["message"].(string))
		case l["type"] == "event_msg" && p["type"] == "agent_message":
			says = append(says, p["message"].(string))
		case p["type"] == "function_call":
			call = p
		case p["type"] == "function_call_output":
			out = p
		}
	}
	if len(users) != 2 || !strings.HasPrefix(users[0], "My dog is called Biscuit.") || !strings.HasPrefix(users[1], "Use the Read tool") {
		t.Fatalf("users %q", users)
	}
	var args struct{ Cmd, Workdir string }
	if call == nil || call["name"] != "exec_command" || json.Unmarshal([]byte(call["arguments"].(string)), &args) != nil ||
		args.Cmd != "wc -l calc.py" || args.Workdir != carryCwd || out == nil || out["call_id"] != call["call_id"] || out["output"] != "       6 calc.py" {
		t.Fatalf("call %v output %v", call, out)
	}
	joined := strings.Join(says, "\n")
	for _, want := range []string{"[Write]", "[Read]", "[Edit]", "def sub(a, b)", "calc.py has 6 lines.", "Done. Added the `sub(a, b)` function"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("%q not said in %q", want, joined)
		}
	}
	// magpie lists it as Codex's, and reads it as Codex's
	Reset()
	c, ok := Find("codex", r.ID)
	if !ok || c.Cwd != carryCwd || !strings.HasPrefix(c.Title, "My dog is called Biscuit") || len(c.Models) != 0 {
		t.Fatalf("listed %+v (found %v)", c, ok)
	}
	// asked again for the same file, the same session
	again, err := CarryOver(s, "codex")
	if err != nil || again != r {
		t.Fatalf("again %+v %v, want %+v", again, err, r)
	}
}

// A Codex session is carried on in Claude Code: a new session in the
// project's folder of Claude Code's, its lines chained by parentUuid, the
// shell calls as Bash with their output, Codex's context and reasoning
// left behind, and no line of it counted as a call of a model.
func TestCarryCodexToClaude(t *testing.T) {
	_, src := carrySetup(t)
	before := sum(t, src)
	s, ok := Find("codex", carryCodex)
	if !ok {
		t.Fatal("codex session not found")
	}
	r, err := CarryOver(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	name, _ := claudeProjectName(carryCwd)
	if r.Path != filepath.Join(ClaudeDir(), "projects", name, r.ID+".jsonl") || !strings.Contains(r.Command, "claude --resume "+r.ID) {
		t.Fatalf("result %+v", r)
	}
	if sum(t, src) != before {
		t.Fatal("the source was changed")
	}
	raw := sum(t, r.Path)
	if !strings.HasPrefix(raw, `{"parentUuid":null,"isSidechain":false,"type":"user"`) {
		t.Fatalf("first line %.120s", raw)
	}
	for _, no := range []string{"environment_context", "skills_instructions", "Answer. Also maybe note", "Need edit.", "encrypted_content", "\\u003c"} {
		if strings.Contains(raw, no) {
			t.Fatalf("%q carried over", no)
		}
	}
	lines := readLines(t, r.Path)
	parent := any(nil)
	var users []string
	var bash, ids []string
	results := map[string]string{}
	for _, l := range lines {
		if l["parentUuid"] != parent || l["sessionId"] != r.ID || l["cwd"] != carryCwd {
			t.Fatalf("line %v after %v", l, parent)
		}
		parent = l["uuid"]
		m := l["message"].(map[string]any)
		if l["type"] == "user" {
			if text, ok := m["content"].(string); ok {
				users = append(users, text)
			} else {
				for _, b := range m["content"].([]any) {
					b := b.(map[string]any)
					results[b["tool_use_id"].(string)] = b["content"].(string)
				}
			}
			continue
		}
		if m["model"] != "<synthetic>" {
			t.Fatalf("assistant line %v", m)
		}
		for _, b := range m["content"].([]any) {
			b := b.(map[string]any)
			if b["type"] == "tool_use" {
				in := b["input"].(map[string]any)
				if b["name"] != "Bash" {
					t.Fatalf("tool %v", b)
				}
				bash = append(bash, in["command"].(string))
				ids = append(ids, b["id"].(string))
			}
		}
	}
	if len(users) != 2 || !strings.HasPrefix(users[0], "My favourite colour is teal.") || !strings.HasPrefix(users[1], "Now add a function mul") {
		t.Fatalf("users %q", users)
	}
	if len(bash) != 2 || bash[0] != "cat calc.py" || !strings.HasPrefix(bash[1], "apply_patch <<'PATCH'") || len(results) != 2 {
		t.Fatalf("bash %q results %q", bash, results)
	}
	for _, id := range ids {
		if !strings.Contains(results[id], "def add(a, b):") {
			t.Fatalf("result of %s: %q", id, results[id])
		}
	}
	// magpie lists it as Claude Code's, with nothing spent
	Reset()
	c, ok := Find("claude", r.ID)
	if !ok || c.Cwd != carryCwd || !strings.HasPrefix(c.Title, "My favourite colour is teal") || len(c.Models) != 0 || c.Cost != 0 {
		t.Fatalf("listed %+v (found %v)", c, ok)
	}
	// and it is carried back the way it came
	if want := []Carry{{Agent: "codex", Convert: true}, {Agent: "omp", Convert: true}}; !reflect.DeepEqual(c.Carry, want) {
		t.Fatalf("carry back %+v", c.Carry)
	}
}

// Nothing is offered for a session that can't be read here, one in WSL, or
// for an agent that isn't installed; and only what carries offers is made.
func TestCarryConvertOffered(t *testing.T) {
	carrySetup(t)
	base := Session{Agent: "claude", ID: "c", Cwd: "/w", Path: "/w/c.jsonl"}
	if c, want := carries(base), []Carry{{Agent: "codex", Convert: true}, {Agent: "omp", Convert: true}}; !reflect.DeepEqual(c, want) {
		t.Fatalf("carries %+v", c)
	}
	codex := Session{Agent: "codex", ID: "x", Cwd: "/w", Path: "/w/x.jsonl"}
	if c, want := carries(codex), []Carry{{Agent: "claude", Convert: true}, {Agent: "omp", Convert: true}}; !reflect.DeepEqual(c, want) {
		t.Fatalf("codex carries %+v", c)
	}
	for _, s := range []Session{
		{Agent: "claude", ID: "c", Cwd: "/w", Path: "/w/c.jsonl", ReadOnly: true},
		{Agent: "claude", ID: "c", Cwd: "/w", Path: "/w/c.jsonl", WSL: "Ubuntu"},
		{Agent: "claude", ID: "c", Path: "/w/c.jsonl"},
		{Agent: "claude", ID: "c", Cwd: "/w"},
		{Agent: "opencode", ID: "o", Cwd: "/w", Path: "/w/o"},
	} {
		if c := carries(s); c != nil {
			t.Fatalf("%+v carried: %+v", s, c)
		}
	}
	agentThere = func(a string) bool { return a != "codex" }
	if c, want := carries(base), []Carry{{Agent: "omp", Convert: true}}; !reflect.DeepEqual(c, want) {
		t.Fatalf("carried with no codex: %+v", c)
	}
	if _, err := CarryOver(base, "codex"); err == nil {
		t.Fatal("converted what wasn't offered")
	}
	agentThere = func(a string) bool { return a != "claude" }
	if c, want := carries(codex), []Carry{{Agent: "omp", Convert: true}}; !reflect.DeepEqual(c, want) {
		t.Fatalf("codex carried with no claude: %+v", c)
	}
	if _, err := CarryOver(codex, "claude"); err == nil {
		t.Fatal("converted for an unavailable claude")
	}
	agentThere = func(a string) bool { return a != "omp" }
	if c, want := carries(base), []Carry{{Agent: "codex", Convert: true}}; !reflect.DeepEqual(c, want) {
		t.Fatalf("carried with no omp: %+v", c)
	}
	if c, want := carries(codex), []Carry{{Agent: "claude", Convert: true}}; !reflect.DeepEqual(c, want) {
		t.Fatalf("codex carried with no omp: %+v", c)
	}
	if _, err := CarryOver(codex, "omp"); err == nil {
		t.Fatal("converted for an unavailable omp")
	}
	agentThere = func(string) bool { return true }
	if _, err := CarryOver(base, "opencode"); err == nil {
		t.Fatal("converted for an unsupported agent")
	}
	agentThere = func(string) bool { return false }
	if c := carries(base); c != nil {
		t.Fatalf("carried with no destinations: %+v", c)
	}
}

func TestCarryOmpConvertOffered(t *testing.T) {
	carrySetup(t)
	omp := Session{Agent: "omp", ID: "o", Cwd: "/w", Path: "/w/o.jsonl"}
	for _, tc := range []struct {
		name   string
		claude bool
		codex  bool
		omp    bool
		want   []Carry
	}{
		{"both", true, true, true, []Carry{{Agent: "claude", Convert: true}, {Agent: "codex", Convert: true}}},
		{"claude only", true, false, true, []Carry{{Agent: "claude", Convert: true}}},
		{"codex only", false, true, true, []Carry{{Agent: "codex", Convert: true}}},
		{"neither", false, false, true, nil},
		{"source not installed", true, true, false, []Carry{{Agent: "claude", Convert: true}, {Agent: "codex", Convert: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agentThere = func(agent string) bool {
				switch agent {
				case "claude":
					return tc.claude
				case "codex":
					return tc.codex
				case "omp":
					return tc.omp
				}
				return false
			}
			if got := carries(omp); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("offers %+v, want %+v", got, tc.want)
			}
			if _, err := CarryOver(omp, "omp"); err == nil {
				t.Fatal("OMP source offered itself")
			}
			for _, target := range []struct {
				name      string
				installed bool
			}{{"claude", tc.claude}, {"codex", tc.codex}} {
				if !target.installed {
					if _, err := CarryOver(omp, target.name); err == nil {
						t.Fatalf("converted for unavailable %s", target.name)
					}
				}
			}
		})
	}
}

// A pasted image goes over as the target's own image, and a compaction
// stands for what came before it: the history Codex kept in its place.
func TestCarryImagesAndCompaction(t *testing.T) {
	carrySetup(t)
	dir := t.TempDir()
	img := "iVBORw0KGgo="
	cc := filepath.Join(dir, "c.jsonl")
	os.WriteFile(cc, []byte(`{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+img+`"}}]},"uuid":"a","timestamp":"2026-10-25T03:50:25.170Z"}`+"\n"+
		`{"parentUuid":"a","isSidechain":false,"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"sig"},{"type":"text","text":"a cat"}]},"uuid":"b","timestamp":"2026-10-25T03:50:26.170Z"}`+"\n"+
		`{"parentUuid":"b","isSidechain":true,"type":"user","message":{"role":"user","content":"side"},"uuid":"s","timestamp":"2026-10-25T03:50:27.170Z"}`+"\n"), 0o644)
	items, err := claudeCarryItems(cc)
	if err != nil {
		t.Fatal(err)
	}
	want := []carryContent{{Type: "text", Text: "what is this?"}, {Type: "image", Data: img, MimeType: "image/png"}}
	if len(items) != 2 || !reflect.DeepEqual(items[0].Content, want) || items[1].Text != "a cat" {
		t.Fatalf("items %+v", items)
	}
	r, err := writeCodexCarry(dir, "/w", items, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, err := codexCarryItems(r.Path)
	if err != nil || len(got) != 2 || !reflect.DeepEqual(got[0].Content, want) || got[1].Text != "a cat" {
		t.Fatalf("codex read back %+v %v", got, err)
	}

	cx := filepath.Join(dir, "x.jsonl")
	os.WriteFile(cx, []byte(`{"timestamp":"2026-10-25T03:48:11.208Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first, long ago"}]}}`+"\n"+
		`{"timestamp":"2026-10-25T03:48:12.208Z","type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"kept"}]},{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,`+img+`"}]}]}}`+"\n"+
		`{"timestamp":"2026-10-25T03:48:13.208Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after"}]}}`+"\n"), 0o644)
	got, err = codexCarryItems(cx)
	kept := []carryContent{{Type: "text", Text: "kept"}}
	image := []carryContent{{Type: "image", Data: img, MimeType: "image/png"}}
	if err != nil || len(got) != 3 || !reflect.DeepEqual(got[0].Content, kept) || !reflect.DeepEqual(got[1].Content, image) || got[2].Text != "after" {
		t.Fatalf("compacted %+v %v", got, err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	r, err = writeClaudeCarry("/w", got, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	back, err := claudeCarryItems(r.Path)
	if err != nil || len(back) != 3 || !reflect.DeepEqual(back[0].Content, kept) || !reflect.DeepEqual(back[1].Content, image) || back[2].Text != "after" {
		t.Fatalf("claude read back %+v %v", back, err)
	}
}

func TestCarryInterleavedUserContent(t *testing.T) {
	carrySetup(t)
	const png = "iVBORw0KGgo="
	const jpeg = "anBn"
	want := []carryContent{
		{Type: "text", Text: "Before"},
		{Type: "image", Data: png, MimeType: "image/png"},
		{Type: "text", Text: "Between"},
		{Type: "image", Data: jpeg, MimeType: "image/jpeg"},
		{Type: "text", Text: "After"},
	}
	wantClaude := []any{
		map[string]any{"type": "text", "text": "Before"},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": png}},
		map[string]any{"type": "text", "text": "Between"},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": jpeg}},
		map[string]any{"type": "text", "text": "After"},
	}
	wantCodex := []any{
		map[string]any{"type": "input_text", "text": "Before"},
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + png},
		map[string]any{"type": "input_text", "text": "Between"},
		map[string]any{"type": "input_image", "image_url": "data:image/jpeg;base64," + jpeg},
		map[string]any{"type": "input_text", "text": "After"},
	}
	wantOmp := []any{
		map[string]any{"type": "text", "text": "Before"},
		map[string]any{"type": "image", "data": png, "mimeType": "image/png"},
		map[string]any{"type": "text", "text": "Between"},
		map[string]any{"type": "image", "data": jpeg, "mimeType": "image/jpeg"},
		map[string]any{"type": "text", "text": "After"},
	}
	sources := []struct {
		name string
		line string
		read func(string) ([]carryItem, error)
	}{
		{"claude", `{"parentUuid":null,"type":"user","message":{"role":"user","content":[{"type":"text","text":"Before"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png + `"}},{"type":"text","text":"Between"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` + jpeg + `"}},{"type":"text","text":"After"}]},"uuid":"a","timestamp":"2026-10-25T03:50:25.170Z"}` + "\n", claudeCarryItems},
		{"codex", `{"timestamp":"2026-10-25T03:50:25.170Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Before"},{"type":"input_image","image_url":"data:image/png;base64,` + png + `"},{"type":"input_text","text":"Between"},{"type":"input_image","image_url":"data:image/jpeg;base64,` + jpeg + `"},{"type":"input_text","text":"After"}]}}` + "\n", codexCarryItems},
	}
	targets := []struct {
		name  string
		write func([]carryItem) (CarryResult, error)
		read  func(string) ([]carryItem, error)
	}{
		{"claude", func(items []carryItem) (CarryResult, error) { return writeClaudeCarry("/w", items, time.Now()) }, claudeCarryItems},
		{"codex", func(items []carryItem) (CarryResult, error) {
			return writeCodexCarry(CodexDir(), "/w", items, time.Now())
		}, codexCarryItems},
		{"omp", func(items []carryItem) (CarryResult, error) { return writeOmpCarry("/w", items, time.Now()) }, ompCarryItems},
	}
	for _, source := range sources {
		t.Run("read/"+source.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.jsonl")
			if err := os.WriteFile(path, []byte(source.line), 0o600); err != nil {
				t.Fatal(err)
			}
			items, err := source.read(path)
			if err != nil || len(items) != 1 || items[0].Kind != "user" || !reflect.DeepEqual(items[0].Content, want) {
				t.Fatalf("source content = %+v, %v", items, err)
			}
		})
	}
	for _, target := range targets {
		t.Run("write/"+target.name, func(t *testing.T) {
			r, err := target.write([]carryItem{{Kind: "user", Content: want}})
			if err != nil {
				t.Fatal(err)
			}
			lines := readLines(t, r.Path)
			switch target.name {
			case "claude":
				message := lines[0]["message"].(map[string]any)
				if got := message["content"]; !reflect.DeepEqual(got, wantClaude) {
					t.Fatalf("Claude content = %#v", got)
				}
			case "codex":
				payload := lines[1]["payload"].(map[string]any)
				if lines[1]["type"] != "response_item" || payload["role"] != "user" || !reflect.DeepEqual(payload["content"], wantCodex) {
					t.Fatalf("Codex response item = %#v", lines[1])
				}
				event := lines[2]["payload"].(map[string]any)
				images := []any{"data:image/png;base64," + png, "data:image/jpeg;base64," + jpeg}
				if lines[2]["type"] != "event_msg" || event["type"] != "user_message" || event["message"] != "Before\nBetween\nAfter" || !reflect.DeepEqual(event["images"], images) {
					t.Fatalf("Codex user event = %#v", lines[2])
				}
			case "omp":
				message := lines[2]["message"].(map[string]any)
				if got := message["content"]; !reflect.DeepEqual(got, wantOmp) {
					t.Fatalf("OMP content = %#v", got)
				}
			}
			back, err := target.read(r.Path)
			if err != nil || len(back) != 1 || back[0].Kind != "user" || !reflect.DeepEqual(back[0].Content, want) {
				t.Fatalf("readback = %+v, %v", back, err)
			}
		})
	}
}

func TestCarryCodexSummaryContent(t *testing.T) {
	carrySetup(t)
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{"compacted", `{"type":"compacted","payload":{"message":"portable summary"}}` + "\n", codexsummary.Prefix + "\nportable summary"},
		{"magpie compaction", `{"type":"response_item","payload":{"type":"compaction","encrypted_content":"` + codexsummary.Encrypted("portable summary") + `"}}` + "\n", codexsummary.Prefix + "\nportable summary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "summary.jsonl")
			if err := os.WriteFile(path, []byte(tc.line), 0o600); err != nil {
				t.Fatal(err)
			}
			items, err := codexCarryItems(path)
			want := []carryContent{{Type: "text", Text: tc.want}}
			if err != nil || len(items) != 1 || items[0].Kind != "user" || !reflect.DeepEqual(items[0].Content, want) {
				t.Fatalf("summary = %+v, %v", items, err)
			}
			r, err := writeClaudeCarry("/w", items, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			back, err := claudeCarryItems(r.Path)
			if err != nil || len(back) != 1 || !reflect.DeepEqual(back[0].Content, want) {
				t.Fatalf("summary readback = %+v, %v", back, err)
			}
		})
	}
}

func TestCarryCodexShellErrorOutput(t *testing.T) {
	carrySetup(t)
	path := filepath.Join(t.TempDir(), "errors.jsonl")
	source := `{"parentUuid":null,"type":"user","message":{"role":"user","content":"run both"},"uuid":"a"}` + "\n" +
		`{"parentUuid":"a","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"bad","name":"Bash","input":{"command":"false"}},{"type":"tool_use","id":"good","name":"Bash","input":{"command":"true"}}]},"uuid":"b"}` + "\n" +
		`{"parentUuid":"b","type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"bad","content":"command failed","is_error":true},{"type":"tool_result","tool_use_id":"good","content":"success","is_error":false}]},"uuid":"c"}` + "\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := claudeCarryItems(path)
	if err != nil {
		t.Fatal(err)
	}
	omp, err := writeOmpCarry(carryCwd, pairCarry(items), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []Session{
		{Agent: "claude", ID: "errors", Cwd: carryCwd, Path: path},
		{Agent: "omp", ID: omp.ID, Cwd: carryCwd, Path: omp.Path},
	} {
		t.Run(source.Agent, func(t *testing.T) {
			r, err := CarryOver(source, "codex")
			if err != nil {
				t.Fatal(err)
			}
			var outputs []string
			for _, line := range readLines(t, r.Path) {
				if line["type"] != "response_item" {
					continue
				}
				payload := line["payload"].(map[string]any)
				if payload["type"] == "function_call_output" {
					outputs = append(outputs, payload["output"].(string))
				}
			}
			want := []string{"[error]\ncommand failed", "success"}
			if !reflect.DeepEqual(outputs, want) {
				t.Fatalf("Codex output = %q, want %q", outputs, want)
			}
			back, err := codexCarryItems(r.Path)
			if err != nil {
				t.Fatal(err)
			}
			var readback []string
			for _, item := range back {
				if item.Kind == "result" {
					readback = append(readback, item.Text)
				}
			}
			if !reflect.DeepEqual(readback, want) {
				t.Fatalf("Codex consumer output = %q, want %q", readback, want)
			}
		})
	}
}
