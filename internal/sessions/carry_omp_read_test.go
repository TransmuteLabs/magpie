package sessions

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const ompReadStamp = "2026-10-25T11:00:00.000Z"

type ompReadMap = map[string]any

func ompReadNode(id, parent, kind string, fields ompReadMap) ompReadMap {
	var p any
	if parent != "" {
		p = parent
	}
	node := ompReadMap{"type": kind, "id": id, "parentId": p, "timestamp": ompReadStamp}
	for key, value := range fields {
		node[key] = value
	}
	return node
}

func ompReadMessage(id, parent, role string, fields ompReadMap) ompReadMap {
	message := ompReadMap{"role": role, "timestamp": int64(1792926000000)}
	for key, value := range fields {
		message[key] = value
	}
	return ompReadNode(id, parent, "message", ompReadMap{"message": message})
}

func ompReadJournal(t *testing.T, path string, entries ...ompReadMap) string {
	t.Helper()
	if path == "" {
		path = filepath.Join(t.TempDir(), "owner", "sessions", "--work--", "2026-10-25T11-00-00-000Z_source.jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, entry := range append([]ompReadMap{
		{"type": "title", "v": 1, "title": "", "updatedAt": ompReadStamp, "pad": ""},
		{"type": "session", "version": 3, "id": "source", "timestamp": ompReadStamp, "cwd": carryCwd},
	}, entries...) {
		b, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func ompReadSession(path string) Session {
	return Session{Agent: "omp", ID: "source", Path: path, Cwd: carryCwd}
}

func ompReadOutput(t *testing.T, source, target string) (CarryResult, []carryItem) {
	t.Helper()
	s := ompReadSession(source)
	r, err := CarryOver(s, target)
	if err != nil {
		t.Fatalf("omp to %s: %v", target, err)
	}
	again, err := CarryOver(s, target)
	if err != nil || again != r {
		t.Fatalf("omp to %s repeated: %+v, %v; want %+v", target, again, err, r)
	}
	var items []carryItem
	if target == "claude" {
		items, err = claudeCarryItems(r.Path)
	} else {
		items, err = codexCarryItems(r.Path)
	}
	if err != nil {
		t.Fatalf("reading %s output: %v", target, err)
	}
	return r, pairCarry(items)
}

func ompReadWords(items []carryItem) string {
	var words []string
	for _, item := range items {
		for _, block := range item.Content {
			if block.Type == "text" {
				words = append(words, block.Text)
			}
		}
		words = append(words, item.Text, item.Input, item.Shell, item.Result)
	}
	return strings.Join(words, "\n")
}

func ompReadWantWords(t *testing.T, items []carryItem, want, absent []string) {
	t.Helper()
	words := ompReadWords(items)
	for _, text := range want {
		if !strings.Contains(words, text) {
			t.Errorf("missing %q in %q", text, words)
		}
	}
	for _, text := range absent {
		if strings.Contains(words, text) {
			t.Errorf("unexpected %q in %q", text, words)
		}
	}
}

func ompReadFiles(t *testing.T) map[string]bool {
	t.Helper()
	files := make(map[string]bool)
	for _, root := range []string{ClaudeDir(), CodexDir()} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
				files[path] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func ompReadFailUnpublished(t *testing.T, source, contains string) {
	t.Helper()
	before := ompReadFiles(t)
	for _, target := range []string{"claude", "codex"} {
		_, err := CarryOver(ompReadSession(source), target)
		if err == nil || !strings.Contains(err.Error(), contains) {
			t.Errorf("omp to %s: got %v, want error containing %q", target, err, contains)
		}
	}
	if after := ompReadFiles(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("failed conversion published a journal: before %v after %v", before, after)
	}
}

func TestCarryOmpRoundTripFixtures(t *testing.T) {
	for _, tc := range []struct {
		agent, id string
		want      []string
		absent    []string
	}{
		{"claude", carryClaude, []string{"My dog is called Biscuit.", "wc -l calc.py", "calc.py has 6 lines."}, []string{"The user wants me to", "thinkingSignature"}},
		{"codex", carryCodex, []string{"My favourite colour is teal.", "cat calc.py", "def add(a, b)"}, []string{"Answer. Also maybe note", "encrypted_content"}},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			claude, codex := carrySetup(t)
			original := claude
			if tc.agent == "codex" {
				original = codex
			}
			before := sum(t, original)
			s, ok := Find(tc.agent, tc.id)
			if !ok {
				t.Fatalf("%s fixture not found", tc.agent)
			}
			forward, err := CarryOver(s, "omp")
			if err != nil {
				t.Fatal(err)
			}
			ompBefore := sum(t, forward.Path)
			for _, target := range []string{"claude", "codex"} {
				t.Run(target, func(t *testing.T) {
					r, items := ompReadOutput(t, forward.Path, target)
					ompReadWantWords(t, items, tc.want, tc.absent)
					Reset()
					listed, ok := Find(target, r.ID)
					if !ok || len(listed.Models) != 0 || listed.Tokens != (Tokens{}) || listed.Cost != 0 {
						t.Fatalf("%s imported accounting: %+v, found %v", target, listed, ok)
					}
				})
			}
			if sum(t, original) != before || sum(t, forward.Path) != ompBefore {
				t.Fatal("round trip modified a source journal")
			}
		})
	}
}

func TestCarryOmpRealV3Fixture(t *testing.T) {
	carrySetup(t)
	path := "testdata/omp/agent/sessions/--work-omp--/2026-09-28T08-00-00-000Z_019a0000-0000-7000-8000-00000000000a.jsonl"
	before := sum(t, path)
	for _, target := range []string{"claude", "codex"} {
		t.Run(target, func(t *testing.T) {
			_, items := ompReadOutput(t, path, target)
			ompReadWantWords(t, items, []string{"Ported.", "ok"}, []string{"Port the parser", "toolu_1"})
		})
	}
	if sum(t, path) != before {
		t.Fatal("native fixture changed")
	}
}

func TestCarryOmpActiveLeafAndParents(t *testing.T) {
	for _, tc := range []struct {
		name, active string
		null         bool
		want         string
	}{
		{"captured", "kept", false, "kept branch"},
		{"missing captured fallback", "unavailable", false, "kept branch"},
		{"explicit self", "late", false, "abandoned branch"},
		{"null cleared", "", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			carrySetup(t)
			last := ompReadNode("late", "abandoned", "model_usage", ompReadMap{"activeLeafId": tc.active, "details": "metadata is not a message"})
			if tc.null {
				last["activeLeafId"] = nil
			}
			path := ompReadJournal(t, "",
				ompReadMessage("root", "", "user", ompReadMap{"content": "root request"}),
				ompReadMessage("abandoned", "root", "user", ompReadMap{"content": "abandoned branch"}),
				ompReadNode("meta", "root", "model_change", ompReadMap{"details": []any{"arbitrary metadata"}}),
				ompReadMessage("kept", "meta", "user", ompReadMap{"content": "kept branch"}),
				last,
			)
			if tc.null {
				ompReadFailUnpublished(t, path, "nothing was said")
				return
			}
			for _, target := range []string{"claude", "codex"} {
				_, items := ompReadOutput(t, path, target)
				other := "abandoned branch"
				if tc.want == other {
					other = "kept branch"
				}
				ompReadWantWords(t, items, []string{"root request", tc.want}, []string{other})
			}
		})
	}
	for _, tc := range []struct {
		name, parent, err string
	}{
		{"missing parent", "missing", "missing parent"},
		{"cycle", "two", "parent cycle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			carrySetup(t)
			path := ompReadJournal(t, "", ompReadMessage("one", tc.parent, "user", ompReadMap{"content": "request"}),
				ompReadNode("two", "one", "model_change", nil))
			ompReadFailUnpublished(t, path, tc.err)
		})
	}
	t.Run("empty parent id", func(t *testing.T) {
		carrySetup(t)
		entry := ompReadMessage("one", "", "user", ompReadMap{"content": "request"})
		entry["parentId"] = ""
		path := ompReadJournal(t, "", entry)
		ompReadFailUnpublished(t, path, "missing parent")
	})
}

func TestCarryOmpPortableCompactionResetAndSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		compaction ompReadMap
		want, gone []string
	}{
		{"summary retained tail", ompReadMap{"summary": "safe summary", "firstKeptEntryId": "kept"}, []string{"safe summary", "retained request", "after compact", "branch context"}, []string{"before reset", "before reset summary", "discarded request"}},
		{"empty with retained tail", ompReadMap{"summary": "", "firstKeptEntryId": "kept"}, []string{"retained request", "after compact", "branch context"}, []string{"before reset", "discarded request", "safe summary"}},
		{"empty still cuts", ompReadMap{"summary": "", "firstKeptEntryId": ""}, []string{"after compact", "branch context"}, []string{"before reset", "before reset summary", "discarded request", "retained request", "safe summary"}},
		{"anthropic through", ompReadMap{"summary": "snapshot summary", "firstKeptEntryId": "", "providerReplayThroughEntryId": "old", "preserveData": ompReadMap{"anthropicCompaction": ompReadMap{"provider": "anthropic", "content": "snapshot secret", "signature": "snapshot signature", "encryptedContent": "snapshot encrypted"}}}, []string{"snapshot summary", "retained request", "after compact"}, []string{"before reset", "before reset summary", "discarded request", "snapshot secret", "snapshot signature", "snapshot encrypted"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			carrySetup(t)
			missing := ompReadMap{"type": "image", "mimeType": "image/png", "data": "blob:sha256:" + strings.Repeat("0", 64)}
			path := ompReadJournal(t, "",
				ompReadMessage("before", "", "user", ompReadMap{"content": []any{ompReadMap{"type": "text", "text": "before reset"}, missing}}),
				ompReadNode("precompact", "before", "compaction", ompReadMap{"summary": "before reset summary", "firstKeptEntryId": ""}),
				ompReadNode("reset", "precompact", "reset_boundary", nil),
				ompReadMessage("old", "reset", "user", ompReadMap{"content": []any{ompReadMap{"type": "text", "text": "discarded request"}, missing}}),
				ompReadMessage("kept", "old", "user", ompReadMap{"content": "retained request"}),
				ompReadNode("compact", "kept", "compaction", tc.compaction),
				ompReadNode("branch", "compact", "branch_summary", ompReadMap{"summary": "branch context", "fromId": "old"}),
				ompReadNode("unsafe-branch", "branch", "branch_summary", ompReadMap{"summary": "¶think: branch secret", "fromId": "old"}),
				ompReadMessage("after", "unsafe-branch", "user", ompReadMap{"content": "after compact"}),
			)
			for _, target := range []string{"claude", "codex"} {
				r, items := ompReadOutput(t, path, target)
				ompReadWantWords(t, items, tc.want, tc.gone)
				for _, hidden := range tc.gone {
					if strings.Contains(sum(t, r.Path), hidden) {
						t.Fatalf("%s journal contains discarded %q", target, hidden)
					}
				}
				if strings.Contains(sum(t, r.Path), "branch secret") {
					t.Fatalf("%s journal contains branch reasoning", target)
				}
				words := ompReadWords(items)
				if tc.compaction["summary"] != "" && strings.Index(words, fmt.Sprint(tc.compaction["summary"])) > strings.Index(words, "retained request") {
					t.Fatalf("summary is not before retained tail: %s", words)
				}
			}
		})
	}
}

func TestCarryOmpNonportableCompactionsReexpand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields ompReadMap
	}{
		{"plain thinking summary", ompReadMap{"summary": "folded ¶think: hidden reasoning"}},
		{"remote provider", ompReadMap{"summary": "provider secret", "preserveData": ompReadMap{"openaiRemoteCompaction": ompReadMap{"replacementHistory": []any{"encrypted replay"}}}}},
		{"snapshot preserve", ompReadMap{"summary": "snapshot secret", "preserveData": ompReadMap{"snapcompact": ompReadMap{"archive": ompReadMap{"text": "archive secret"}}}}},
		{"snapshot method", ompReadMap{"summary": "snapshot secret", "method": "snapcompact"}},
		{"notes rollover", ompReadMap{"summary": "not a summary", "details": ompReadMap{"kind": "experimental-context-rollover"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			carrySetup(t)
			tc.fields["firstKeptEntryId"] = "recent"
			path := ompReadJournal(t, "",
				ompReadMessage("old", "", "user", ompReadMap{"content": "original request"}),
				ompReadMessage("recent", "old", "assistant", ompReadMap{"content": []any{ompReadMap{"type": "text", "text": "visible answer"}}}),
				ompReadNode("unsafe", "recent", "compaction", tc.fields),
				ompReadMessage("after", "unsafe", "user", ompReadMap{"content": "new request"}),
			)
			for _, target := range []string{"claude", "codex"} {
				r, items := ompReadOutput(t, path, target)
				ompReadWantWords(t, items, []string{"original request", "visible answer", "new request"}, []string{"hidden reasoning", "provider secret", "encrypted replay", "archive secret", "snapshot secret", "not a summary"})
				for _, secret := range []string{"hidden reasoning", "provider secret", "encrypted replay", "archive secret", "snapshot secret", "not a summary"} {
					if strings.Contains(sum(t, r.Path), secret) {
						t.Fatalf("%s leaked %q", target, secret)
					}
				}
			}
		})
	}
	// A later opaque boundary must not displace an earlier safe one.
	t.Run("previous portable boundary", func(t *testing.T) {
		carrySetup(t)
		path := ompReadJournal(t, "",
			ompReadMessage("old", "", "user", ompReadMap{"content": "obsolete original"}),
			ompReadNode("safe", "old", "compaction", ompReadMap{"summary": "earlier summary", "firstKeptEntryId": ""}),
			ompReadMessage("request", "safe", "user", ompReadMap{"content": "preserved request"}),
			ompReadNode("opaque", "request", "compaction", ompReadMap{"summary": "¶think: hidden", "firstKeptEntryId": ""}),
			ompReadMessage("after", "opaque", "assistant", ompReadMap{"content": []any{ompReadMap{"type": "text", "text": "visible"}}}),
		)
		for _, target := range []string{"claude", "codex"} {
			_, items := ompReadOutput(t, path, target)
			ompReadWantWords(t, items, []string{"earlier summary", "preserved request", "visible"}, []string{"obsolete original", "hidden"})
		}
	})
}

func TestCarryOmpCompactionRetainedBeforeReset(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields ompReadMap
	}{
		{"first kept", ompReadMap{"summary": "safe summary", "firstKeptEntryId": "before"}},
		{"snapshot through", ompReadMap{"summary": "safe summary", "firstKeptEntryId": "", "providerReplayThroughEntryId": "before", "preserveData": ompReadMap{"anthropicCompaction": ompReadMap{"provider": "anthropic", "content": "snapshot secret"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			carrySetup(t)
			path := ompReadJournal(t, "",
				ompReadMessage("before", "", "user", ompReadMap{"content": "old before reset"}),
				ompReadNode("reset", "before", "reset_boundary", nil),
				ompReadMessage("after", "reset", "user", ompReadMap{"content": "retained after reset"}),
				ompReadNode("compact", "after", "compaction", tc.fields),
			)
			for _, target := range []string{"claude", "codex"} {
				_, items := ompReadOutput(t, path, target)
				ompReadWantWords(t, items, []string{"safe summary", "retained after reset"}, []string{"old before reset", "snapshot secret"})
			}
		})
	}
}

func TestCarryOmpMessagesAndCalls(t *testing.T) {
	carrySetup(t)
	path := ompReadJournal(t, "",
		ompReadMessage("u", "", "user", ompReadMap{"content": "human request"}),
		ompReadMessage("agent", "u", "user", ompReadMap{"content": []any{ompReadMap{"type": "text", "text": "agent injection"}, ompReadMap{"type": "image", "mimeType": "image/png", "data": "blob:sha256:invalid"}}, "attribution": "agent"}),
		ompReadMessage("synth", "agent", "user", ompReadMap{"content": []any{ompReadMap{"type": "text", "text": "synthetic injection"}, ompReadMap{"type": "image", "mimeType": "image/png", "data": "blob:sha256:invalid"}}, "synthetic": true}),
		ompReadMessage("developer", "synth", "developer", ompReadMap{"content": "developer secret"}),
		ompReadMessage("system", "developer", "system", ompReadMap{"content": "system secret"}),
		ompReadNode("extension", "system", "custom_message", ompReadMap{"content": "extension secret", "attribution": "user"}),
		ompReadMessage("assistant", "extension", "assistant", ompReadMap{"content": []any{
			ompReadMap{"type": "text", "text": "before calls"},
			ompReadMap{"type": "thinking", "thinking": "private reasoning", "thinkingSignature": "private signature"},
			ompReadMap{"type": "thinking", "thinking": "visible narration", "narration": true, "thinkingSignature": "narration signature"},
			ompReadMap{"type": "redactedThinking", "data": "opaque secret"},
			ompReadMap{"type": "anthropicServerTool", "block": ompReadMap{"type": "server_tool_use", "input": "server secret"}},
			ompReadMap{"type": "toolCall", "id": "shell", "name": "bash", "arguments": ompReadMap{"command": "printf shell", "cwd": "/tmp/other place"}},
			ompReadMap{"type": "toolCall", "id": "read", "name": "read", "arguments": ompReadMap{"path": "sample.txt"}},
			ompReadMap{"type": "toolCall", "id": "\x00omp-execution:bash-exec", "name": "inspect", "input": ompReadMap{"path": "collision.txt"}},
			ompReadMap{"type": "toolCall", "id": "dangling", "name": "bash", "arguments": ompReadMap{"command": "never completed"}},
			ompReadMap{"type": "text", "text": "after calls"},
		}}),
		ompReadMessage("shell-result", "assistant", "toolResult", ompReadMap{"toolCallId": "shell", "isError": true, "content": []any{ompReadMap{"type": "text", "text": "shell failed"}, ompReadMap{"type": "image", "data": "secret tool image"}}}),
		ompReadMessage("read-result", "shell-result", "toolResult", ompReadMap{"toolCallId": "read", "content": []any{ompReadMap{"type": "text", "text": "read output"}}}),
		ompReadMessage("collision-result", "read-result", "toolResult", ompReadMap{"toolCallId": "\x00omp-execution:bash-exec", "content": "collision output"}),
		ompReadMessage("bash-exec", "collision-result", "bashExecution", ompReadMap{"command": "printf standalone", "output": "standalone failure", "exitCode": 9}),
		ompReadMessage("python-exec", "bash-exec", "pythonExecution", ompReadMap{"code": "print(7)", "output": "seven", "cancelled": true}),
		ompReadMessage("excluded", "python-exec", "bashExecution", ompReadMap{"command": "excluded command", "output": "excluded output", "excludeFromContext": true}),
		ompReadMessage("sibling", "assistant", "toolResult", ompReadMap{"toolCallId": "dangling", "content": "sibling-only result"}),
		ompReadNode("late", "sibling", "model_usage", ompReadMap{"activeLeafId": "excluded"}),
	)
	for _, target := range []string{"claude", "codex"} {
		r, items := ompReadOutput(t, path, target)
		ompReadWantWords(t, items,
			[]string{"human request", "before calls", "visible narration", "after calls", "printf shell", "shell failed", "[image]", "[read]", "sample.txt", "read output", "[inspect]", "collision.txt", "collision output", "printf standalone", "standalone failure", "[python]", "print(7)", "seven", "[error]"},
			[]string{"agent injection", "synthetic injection", "developer secret", "system secret", "extension secret", "private reasoning", "private signature", "narration signature", "opaque secret", "server secret", "secret tool image", "never completed", "sibling-only result", "excluded command", "excluded output", "[no output was recorded]"})
		words := ompReadWords(items)
		if strings.Index(words, "before calls") > strings.Index(words, "visible narration") ||
			strings.Index(words, "visible narration") > strings.Index(words, "printf shell") ||
			strings.Index(words, "printf shell") > strings.Index(words, "after calls") {
			t.Fatalf("%s assistant blocks out of order: %s", target, words)
		}
		var shells []carryItem
		for _, item := range items {
			if item.Kind == "call" && item.Shell != "" {
				shells = append(shells, item)
			}
		}
		if len(shells) != 2 || !strings.Contains(shells[0].Shell, "printf shell") || !strings.Contains(shells[1].Shell, "printf standalone") {
			t.Fatalf("%s shells: %+v", target, shells)
		}
		if target == "claude" {
			if !strings.Contains(shells[0].Shell, "/tmp/other place") || !shells[0].IsError || !shells[1].IsError {
				t.Fatalf("claude workdir/error lost: %+v", shells)
			}
		} else {
			if shells[0].Workdir != "/tmp/other place" {
				t.Fatalf("codex workdir lost: %+v", shells)
			}
			if shells[0].Result != "[error]\nshell failed\n[image]" || shells[1].Result != "[error]\nstandalone failure" {
				t.Fatalf("codex error output lost: %+v", shells)
			}
		}
		for _, hidden := range []string{"agent injection", "synthetic injection", "developer secret", "system secret", "extension secret", "private reasoning", "private signature", "narration signature", "opaque secret", "server secret", "secret tool image", "never completed", "sibling-only result", "excluded command", "excluded output"} {
			if strings.Contains(sum(t, r.Path), hidden) {
				t.Fatalf("%s journal contains %q", target, hidden)
			}
		}
	}
}

func TestCarryOmpOrderedImagesAndBlobStores(t *testing.T) {
	carrySetup(t)
	source := filepath.Join(t.TempDir(), "owner", "sessions", "--work--", "source.jsonl")
	sibling := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(source))), "blobs")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	stores := []string{
		sibling,
		filepath.Join(OmpDir(), "blobs"),
		filepath.Join(home, ".omp", "profiles", "work", "agent", "blobs"),
	}
	if runtime.GOOS != "windows" {
		xdg := os.Getenv("XDG_DATA_HOME")
		stores = append(stores, filepath.Join(xdg, "omp", "blobs"), filepath.Join(xdg, "omp", "profiles", "work", "blobs"))
	}
	var files []ompReadMap
	var wantImages []carryContent
	var blobPaths, blobBefore []string
	var blobTimes []time.Time
	for i, store := range stores {
		if err := os.MkdirAll(store, 0o700); err != nil {
			t.Fatal(err)
		}
		hash := strings.Repeat(string("abcdef"[i]), 64)
		raw := fmt.Sprintf("raw-%d", i)
		path := filepath.Join(store, hash)
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		blobPaths, blobBefore = append(blobPaths, path), append(blobBefore, raw)
		blobTimes = append(blobTimes, info.ModTime())
		image := ompReadMap{"type": "image", "mimeType": "image/png", "data": "blob:sha256:" + hash}
		files = append(files, ompReadMap{"path": "secret filename", "content": "secret auto-read content", "image": image})
		wantImages = append(wantImages, carryContent{Type: "image", MimeType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte(raw))})
	}
	// A conflicting hash in the default store must not shadow the source sibling.
	firstHash := strings.Repeat("a", 64)
	if err := os.WriteFile(filepath.Join(stores[1], firstHash), []byte("wrong store"), 0o600); err != nil {
		t.Fatal(err)
	}
	inline := carryContent{Type: "image", MimeType: "image/jpeg", Data: base64.StdEncoding.EncodeToString([]byte("inline"))}
	content := []any{
		ompReadMap{"type": "text", "text": "first text"},
		ompReadMap{"type": "image", "mimeType": inline.MimeType, "data": inline.Data},
		ompReadMap{"type": "text", "text": "second text"},
		ompReadMap{"type": "image", "mimeType": "image/png", "data": "blob:sha256:" + firstHash},
		ompReadMap{"type": "text", "text": "last text"},
	}
	files = append(files, ompReadMap{"path": "text only", "content": "hidden document"})
	path := ompReadJournal(t, source,
		ompReadMessage("user", "", "user", ompReadMap{"content": content}),
		ompReadMessage("mention", "user", "fileMention", ompReadMap{"files": files}),
	)
	before := sum(t, path)
	for _, target := range []string{"claude", "codex"} {
		r, items := ompReadOutput(t, path, target)
		var users [][]carryContent
		for _, item := range items {
			if item.Kind == "user" {
				users = append(users, item.Content)
			}
		}
		first := []carryContent{{Type: "text", Text: "first text"}, inline, {Type: "text", Text: "second text"}, wantImages[0], {Type: "text", Text: "last text"}}
		if len(users) != 2 || !reflect.DeepEqual(users[0], first) || !reflect.DeepEqual(users[1], wantImages) {
			t.Fatalf("%s ordered content: %+v; want %+v, %+v", target, users, first, wantImages)
		}
		for _, hidden := range []string{"secret filename", "secret auto-read content", "hidden document", "wrong store", "blob:sha256:"} {
			if strings.Contains(sum(t, r.Path), hidden) {
				t.Fatalf("%s output contains %q", target, hidden)
			}
		}
	}
	if sum(t, path) != before {
		t.Fatal("image source journal modified")
	}
	for i, path := range blobPaths {
		if sum(t, path) != blobBefore[i] {
			t.Fatalf("blob %s modified", path)
		}
		info, err := os.Stat(path)
		if err != nil || !info.ModTime().Equal(blobTimes[i]) {
			t.Fatalf("blob %s timestamp changed: %v, %v", path, info, err)
		}
	}
}

func TestCarryOmpBlobErrorsAndOffBranch(t *testing.T) {
	for _, tc := range []struct{ name, ref, error string }{
		{"missing", "blob:sha256:" + strings.Repeat("0", 64), "unresolved omp image blob"},
		{"short", "blob:sha256:abcd", "invalid omp image blob"},
		{"uppercase", "blob:sha256:" + strings.Repeat("A", 64), "invalid omp image blob"},
		{"uppercase prefix", "BLOB:sha256:" + strings.Repeat("a", 64), "invalid omp image blob"},
		{"traversal", "blob:sha256:../../private", "invalid omp image blob"},
		{"wrong prefix", "blob:sha512:" + strings.Repeat("a", 64), "invalid omp image blob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			carrySetup(t)
			image := ompReadMap{"type": "image", "mimeType": "image/png", "data": tc.ref}
			path := ompReadJournal(t, "", ompReadMessage("root", "", "user", ompReadMap{"content": "request"}),
				ompReadMessage("image", "root", "user", ompReadMap{"content": []any{image}}))
			before := sum(t, path)
			ompReadFailUnpublished(t, path, tc.error)
			if sum(t, path) != before {
				t.Fatal("failed conversion modified source")
			}
		})
	}
	t.Run("off branch ignored", func(t *testing.T) {
		carrySetup(t)
		missing := ompReadMap{"type": "image", "mimeType": "image/png", "data": "blob:sha256:" + strings.Repeat("0", 64)}
		path := ompReadJournal(t, "", ompReadMessage("root", "", "user", ompReadMap{"content": "original request"}),
			ompReadMessage("abandoned", "root", "user", ompReadMap{"content": []any{missing}}),
			ompReadMessage("kept", "root", "user", ompReadMap{"content": "active request"}))
		for _, target := range []string{"claude", "codex"} {
			_, items := ompReadOutput(t, path, target)
			ompReadWantWords(t, items, []string{"original request", "active request"}, []string{"blob:sha256:"})
		}
	})
	t.Run("unreadable blob", func(t *testing.T) {
		carrySetup(t)
		source := filepath.Join(t.TempDir(), "owner", "sessions", "work", "source.jsonl")
		hash := strings.Repeat("e", 64)
		blob := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(source))), "blobs", hash)
		if err := os.MkdirAll(blob, 0o700); err != nil {
			t.Fatal(err)
		}
		image := ompReadMap{"type": "image", "mimeType": "image/png", "data": "blob:sha256:" + hash}
		path := ompReadJournal(t, source, ompReadMessage("one", "", "user", ompReadMap{"content": []any{image}}))
		ompReadFailUnpublished(t, path, "unresolved omp image blob")
	})
	t.Run("file mention missing blob", func(t *testing.T) {
		carrySetup(t)
		image := ompReadMap{"type": "image", "mimeType": "image/png", "data": "blob:sha256:" + strings.Repeat("e", 64)}
		path := ompReadJournal(t, "",
			ompReadMessage("one", "", "user", ompReadMap{"content": "request"}),
			ompReadMessage("mention", "one", "fileMention", ompReadMap{"files": []any{ompReadMap{"path": "image.png", "content": "ignored", "image": image}}}),
		)
		ompReadFailUnpublished(t, path, "unresolved omp image blob")
	})
	t.Run("relative XDG ignored", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("XDG stores are not used on Windows")
		}
		carrySetup(t)
		xdg := t.TempDir()
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		relative, err := filepath.Rel(cwd, xdg)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("XDG_DATA_HOME", relative)
		hash := strings.Repeat("f", 64)
		dir := filepath.Join(xdg, "omp", "blobs")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, hash), []byte("outside validated environment"), 0o600); err != nil {
			t.Fatal(err)
		}
		image := ompReadMap{"type": "image", "mimeType": "image/png", "data": "blob:sha256:" + hash}
		path := ompReadJournal(t, "", ompReadMessage("one", "", "user", ompReadMap{"content": []any{image}}))
		ompReadFailUnpublished(t, path, "unresolved omp image blob")
	})
}

func TestCarryOmpTimestampAndUnboundedLines(t *testing.T) {
	carrySetup(t)
	first := ompReadMessage("one", "", "user", ompReadMap{"content": "first timestamp", "timestamp": int64(1000)})
	first["timestamp"] = "2026-10-25T12:34:56.000Z"
	second := ompReadMessage("two", "one", "user", ompReadMap{"content": "fallback timestamp", "timestamp": int64(1792926000000)})
	second["timestamp"] = "not a timestamp"
	// A non-conversation metadata line exceeds scanner's usual 32 MiB limit.
	long := ompReadNode("metadata", "two", "custom", ompReadMap{"data": strings.Repeat("x", (32<<20)+1)})
	path := ompReadJournal(t, "", first, second, long)
	wantFirst, _ := time.Parse(time.RFC3339Nano, "2026-10-25T12:34:56.000Z")
	wantSecond := time.UnixMilli(1792926000000)
	for _, target := range []string{"claude", "codex"} {
		_, items := ompReadOutput(t, path, target)
		var times []time.Time
		for _, item := range items {
			if item.Kind == "user" {
				times = append(times, item.Time)
			}
		}
		if len(times) != 2 || !times[0].Equal(wantFirst) || !times[1].Equal(wantSecond) {
			t.Fatalf("%s timestamps: %v, want %v and %v", target, times, wantFirst, wantSecond)
		}
	}
}
