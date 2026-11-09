package sessions

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type ompCarryShell struct {
	command string
	cwd     string
	output  string
	isError bool
}

// Read the journal as an omp consumer does: title slot, header, then a
// branch of messages with matching tool calls and results.
func checkOmpCarryJournal(t *testing.T, r CarryResult, cwd string) []ompCarryShell {
	t.Helper()
	u, err := uuid.Parse(r.ID)
	if err != nil || u.Version() != 7 {
		t.Fatalf("session id %q is not UUIDv7: %v", r.ID, err)
	}
	raw := sum(t, r.Path)
	if n := strings.IndexByte(raw, '\n'); n != 255 {
		t.Fatalf("title slot is %d bytes including newline, want 256", n+1)
	}
	lines := readLines(t, r.Path)
	if len(lines) < 3 {
		t.Fatalf("journal has %d lines", len(lines))
	}
	title, header := lines[0], lines[1]
	if title["type"] != "title" || title["v"] != float64(1) || title["title"] != "" {
		t.Fatalf("title slot: %v", title)
	}
	if header["type"] != "session" || header["version"] != float64(3) || header["id"] != r.ID || header["cwd"] != cwd {
		t.Fatalf("header: %v", header)
	}
	stamp, ok := header["timestamp"].(string)
	if !ok || title["updatedAt"] != stamp {
		t.Fatalf("header/title timestamps: %v / %v", header, title)
	}
	if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
		t.Fatalf("header timestamp %q: %v", stamp, err)
	}
	filename := strings.NewReplacer(":", "-", ".", "-").Replace(stamp) + "_" + r.ID + ".jsonl"
	if filepath.Base(r.Path) != filename {
		t.Fatalf("filename %q, want %q", filepath.Base(r.Path), filename)
	}

	seen := map[string]bool{}
	var parent any
	var calls []ompCarryShell
	pendingID := ""
	for _, entry := range lines[2:] {
		if entry["type"] != "message" || entry["parentId"] != parent {
			t.Fatalf("message branch after %v: %v", parent, entry)
		}
		id, ok := entry["id"].(string)
		if !ok || id == "" || seen[id] {
			t.Fatalf("duplicate or missing entry id: %v", entry)
		}
		seen[id], parent = true, id
		envelope, ok := entry["timestamp"].(string)
		if !ok {
			t.Fatalf("missing envelope timestamp: %v", entry)
		}
		at, err := time.Parse(time.RFC3339Nano, envelope)
		if err != nil {
			t.Fatalf("entry timestamp %q: %v", envelope, err)
		}
		message, ok := entry["message"].(map[string]any)
		if !ok || message["timestamp"] != float64(at.UnixMilli()) {
			t.Fatalf("message timestamp and envelope disagree: %v", entry)
		}
		switch message["role"] {
		case "user":
			if pendingID != "" {
				t.Fatalf("unanswered shell call %q", pendingID)
			}
		case "assistant":
			if pendingID != "" || message["api"] != "anthropic-messages" || message["provider"] != "anthropic" || message["model"] != "<synthetic>" {
				t.Fatalf("assistant message: %v, pending %q", message, pendingID)
			}
			usage, ok := message["usage"].(map[string]any)
			if !ok {
				t.Fatalf("assistant usage: %v", message)
			}
			for _, key := range []string{"input", "output", "cacheRead", "cacheWrite", "totalTokens"} {
				if usage[key] != float64(0) {
					t.Fatalf("usage %s: %v", key, usage)
				}
			}
			cost, ok := usage["cost"].(map[string]any)
			if !ok {
				t.Fatalf("missing cost: %v", usage)
			}
			for _, key := range []string{"input", "output", "cacheRead", "cacheWrite", "total"} {
				if cost[key] != float64(0) {
					t.Fatalf("cost %s: %v", key, cost)
				}
			}
			blocks, ok := message["content"].([]any)
			if !ok || len(blocks) != 1 {
				t.Fatalf("assistant blocks: %v", message)
			}
			block, ok := blocks[0].(map[string]any)
			if !ok {
				t.Fatalf("assistant block: %v", blocks[0])
			}
			switch block["type"] {
			case "text":
				if message["stopReason"] != "stop" {
					t.Fatalf("text stop reason: %v", message)
				}
			case "toolCall":
				args, ok := block["arguments"].(map[string]any)
				if !ok || block["name"] != "bash" || message["stopReason"] != "toolUse" {
					t.Fatalf("native shell call: %v", message)
				}
				pendingID, ok = block["id"].(string)
				if !ok || pendingID == "" {
					t.Fatalf("shell call without id: %v", block)
				}
				command, ok := args["command"].(string)
				if !ok || command == "" {
					t.Fatalf("shell command: %v", args)
				}
				workdir, _ := args["cwd"].(string)
				calls = append(calls, ompCarryShell{command: command, cwd: workdir})
			default:
				t.Fatalf("unexpected assistant block: %v", block)
			}
		case "toolResult":
			if pendingID == "" || message["toolCallId"] != pendingID || message["toolName"] != "bash" {
				t.Fatalf("unpaired shell result: %v, pending %q", message, pendingID)
			}
			blocks, ok := message["content"].([]any)
			if !ok || len(blocks) != 1 {
				t.Fatalf("shell result content: %v", message)
			}
			block, ok := blocks[0].(map[string]any)
			if !ok || block["type"] != "text" {
				t.Fatalf("shell result block: %v", blocks[0])
			}
			output, ok := block["text"].(string)
			if !ok {
				t.Fatalf("shell result text: %v", block)
			}
			isError, ok := message["isError"].(bool)
			if !ok {
				t.Fatalf("shell result error flag: %v", message)
			}
			calls[len(calls)-1].output, calls[len(calls)-1].isError = output, isError
			pendingID = ""
		default:
			t.Fatalf("unexpected message role: %v", message)
		}
	}
	if pendingID != "" {
		t.Fatalf("last shell call %q has no result", pendingID)
	}
	return calls
}

func TestCarryFixturesToOmp(t *testing.T) {
	for _, tc := range []struct {
		agent, sourceID, title string
		words                  []string
		omitted                []string
	}{
		{"claude", carryClaude, "My dog is called Biscuit", []string{"My dog is called Biscuit.", "[Write]", "[Read]", "[Edit]", "calc.py has 6 lines."}, []string{"The user wants me to", `"thinking"`, `"signature"`}},
		{"codex", carryCodex, "My favourite colour is teal", []string{"My favourite colour is teal.", "Now add a function mul", "def add(a, b):"}, []string{"Answer. Also maybe note", "Need edit.", `"reasoning"`, "encrypted_content"}},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			claude, codex := carrySetup(t)
			src := claude
			if tc.agent == "codex" {
				src = codex
			}
			before := sum(t, src)
			root := filepath.Join(OmpDir(), "sessions")
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("unexpected existing omp store: %v", err)
			}
			s, ok := Find(tc.agent, tc.sourceID)
			if !ok {
				t.Fatalf("%s fixture not found", tc.agent)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("listing created an omp store: %v", err)
			}
			r, err := CarryOver(s, "omp")
			if err != nil {
				t.Fatal(err)
			}
			if r.Agent != "omp" || !strings.HasPrefix(r.Path, root+string(filepath.Separator)) || !strings.Contains(r.Command, "omp --resume "+r.ID) {
				t.Fatalf("result: %+v", r)
			}
			calls := checkOmpCarryJournal(t, r, carryCwd)
			if tc.agent == "claude" {
				if len(calls) != 1 || calls[0].command != "wc -l calc.py" || calls[0].cwd != "" || calls[0].output != "       6 calc.py" || calls[0].isError {
					t.Fatalf("claude shell: %+v", calls)
				}
			} else if len(calls) != 2 || calls[0].command != "cat calc.py" || !strings.HasPrefix(calls[1].command, "apply_patch <<'PATCH'") || calls[0].cwd != carryCwd || calls[1].cwd != carryCwd || !strings.Contains(calls[0].output, "def add(a, b):") || !strings.Contains(calls[1].output, "def add(a, b):") || calls[0].isError || calls[1].isError {
				t.Fatalf("codex shell: %+v", calls)
			}
			raw := sum(t, r.Path)
			for _, omitted := range tc.omitted {
				if strings.Contains(raw, omitted) {
					t.Fatalf("private reasoning %q was carried", omitted)
				}
			}
			Reset()
			listed, ok := Find("omp", r.ID)
			if !ok || listed.Cwd != carryCwd || !strings.HasPrefix(listed.Title, tc.title) || len(listed.Models) != 0 || listed.Tokens != (Tokens{}) || listed.Cost != 0 {
				t.Fatalf("omp readback %+v, found %v", listed, ok)
			}
			tr, err := TranscriptOf(listed)
			if err != nil {
				t.Fatal(err)
			}
			text := transcriptText(t, tr)
			for _, word := range tc.words {
				if !strings.Contains(text, word) {
					t.Fatalf("transcript missing %q: %s", word, text)
				}
			}
			for _, call := range Calls(time.Time{}) {
				if call.Agent == "omp" && call.Session == r.ID {
					t.Fatalf("synthetic assistant counted as a call: %+v", call)
				}
			}
			again, err := CarryOver(s, "omp")
			if err != nil || again != r {
				t.Fatalf("repeat = %+v, %v; want %+v", again, err, r)
			}
			if err := os.Remove(r.Path); err != nil {
				t.Fatal(err)
			}
			recreated, err := CarryOver(s, "omp")
			if err != nil || recreated.ID == r.ID || recreated.Path == r.Path {
				t.Fatalf("recreate = %+v, %v; old %+v", recreated, err, r)
			}
			checkOmpCarryJournal(t, recreated, carryCwd)
			if sum(t, src) != before {
				t.Fatal("source was changed")
			}
		})
	}
}

func TestCarryOmpImagesErrorsAndMissingOutput(t *testing.T) {
	carrySetup(t)
	path := filepath.Join(t.TempDir(), "image-error.jsonl")
	const img = "iVBORw0KGgo="
	source := `{"parentUuid":null,"type":"user","message":{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + img + `"}}]},"uuid":"a","timestamp":"2026-10-25T03:50:25.170Z"}` + "\n" +
		`{"parentUuid":"a","type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"private thought","signature":"secret"},{"type":"text","text":"Checking."},{"type":"tool_use","id":"bad","name":"Bash","input":{"command":"false"}},{"type":"tool_use","id":"missing","name":"Bash","input":{"command":"sleep 5"}},{"type":"tool_use","id":"read","name":"Read","input":{"file_path":"missing.txt"}}]},"uuid":"b","timestamp":"2026-10-25T03:50:26.170Z"}` + "\n" +
		`{"parentUuid":"b","type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"bad","content":"command failed","is_error":true},{"type":"tool_result","tool_use_id":"read","content":"missing file","is_error":true}]},"uuid":"c","timestamp":"2026-10-25T03:50:27.170Z"}` + "\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Session{Agent: "claude", ID: "image-error", Cwd: carryCwd, Path: path}
	r, err := CarryOver(s, "omp")
	if err != nil {
		t.Fatal(err)
	}
	calls := checkOmpCarryJournal(t, r, carryCwd)
	if len(calls) != 2 || calls[0] != (ompCarryShell{command: "false", output: "command failed", isError: true}) || calls[1] != (ompCarryShell{command: "sleep 5", output: "[no output was recorded]"}) {
		t.Fatalf("shell calls: %+v", calls)
	}
	lines := readLines(t, r.Path)
	user := lines[2]["message"].(map[string]any)
	blocks := user["content"].([]any)
	if len(blocks) != 2 || blocks[0].(map[string]any)["text"] != "what is this?" {
		t.Fatalf("user content: %v", blocks)
	}
	image := blocks[1].(map[string]any)
	if image["type"] != "image" || image["data"] != img || image["mimeType"] != "image/png" {
		t.Fatalf("user image: %v", image)
	}
	raw := sum(t, r.Path)
	for _, omitted := range []string{"private thought", "secret"} {
		if strings.Contains(raw, omitted) {
			t.Fatalf("reasoning leaked: %q", omitted)
		}
	}
	tr, err := TranscriptOf(Session{Agent: "omp", Path: r.Path})
	if err != nil {
		t.Fatal(err)
	}
	text := transcriptText(t, tr)
	for _, want := range []string{"[image]", "[Read]", "[error]", "missing file", "command failed", "[no output was recorded]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("transcript missing %q: %s", want, text)
		}
	}
	if sum(t, path) != source {
		t.Fatal("source was changed")
	}
}

func TestCarryOmpStorePrecedence(t *testing.T) {
	carrySetup(t)
	dir := t.TempDir()
	xdg, pi := filepath.Join(dir, "data"), filepath.Join(dir, "pi-override")
	t.Setenv("XDG_DATA_HOME", xdg)
	t.Setenv("PI_CODING_AGENT_DIR", pi)
	imageBytes := bytes.Repeat([]byte{0x71}, 768)
	imageData := base64.StdEncoding.EncodeToString(imageBytes)
	imageHash := fmt.Sprintf("%x", sha256.Sum256(imageBytes))
	items := []carryItem{{Kind: "user", Content: []carryContent{{Type: "text", Text: "hello"}, {Type: "image", Data: imageData, MimeType: "image/png"}}}}
	now := time.Date(2026, 10, 25, 3, 50, 25, 123000000, time.UTC)
	first, err := writeOmpCarry(carryCwd, items, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.Path, filepath.Join(OmpDir(), "sessions")+string(filepath.Separator)) {
		t.Fatalf("default destination: %s", first.Path)
	}
	if _, err := os.Stat(filepath.Join(xdg, "omp")); !os.IsNotExist(err) {
		t.Fatalf("uninitialized XDG root was created: %v", err)
	}
	checkOmpCarryJournal(t, first, carryCwd)
	if got, err := os.ReadFile(filepath.Join(OmpDir(), "blobs", imageHash)); err != nil || !bytes.Equal(got, imageBytes) {
		t.Fatalf("default image blob: %d bytes, %v", len(got), err)
	}
	if err := os.MkdirAll(filepath.Join(xdg, "omp"), 0o700); err != nil {
		t.Fatal(err)
	}
	second, err := writeOmpCarry(carryCwd, items, now)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(OmpDir(), "sessions")
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		root = filepath.Join(xdg, "omp", "sessions")
	}
	if !strings.HasPrefix(second.Path, root+string(filepath.Separator)) || strings.HasPrefix(second.Path, pi+string(filepath.Separator)) {
		t.Fatalf("initialized destination: %s, want under %s", second.Path, root)
	}
	checkOmpCarryJournal(t, second, carryCwd)
	if got, err := os.ReadFile(filepath.Join(filepath.Dir(root), "blobs", imageHash)); err != nil || !bytes.Equal(got, imageBytes) {
		t.Fatalf("initialized-store image blob: %d bytes, %v", len(got), err)
	}
	Reset()
	for _, r := range []CarryResult{first, second} {
		if listed, ok := Find("omp", r.ID); !ok || listed.Path != r.Path {
			t.Fatalf("destination not listed: %+v, found %v", listed, ok)
		}
	}
}

func TestCarryOmpExternalizedImages(t *testing.T) {
	carrySetup(t)
	// One source line exceeds the transcript scanner's limit. Once its images
	// are in native blobs the earlier branch remains reachable on readback.
	largeBytes := bytes.Repeat([]byte{0x42}, maxLine*3/4+3)
	large := base64.StdEncoding.EncodeToString(largeBytes)
	if len(large) <= maxLine {
		t.Fatal("image does not exceed the line limit")
	}
	smallBytes := bytes.Repeat([]byte{0x53}, 768)
	small := base64.StdEncoding.EncodeToString(smallBytes) // exactly 1024 characters
	tiny := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x54}, 765))
	largeHash := fmt.Sprintf("%x", sha256.Sum256(largeBytes))
	smallHash := fmt.Sprintf("%x", sha256.Sum256(smallBytes))

	blobs := filepath.Join(OmpDir(), "blobs")
	path := filepath.Join(t.TempDir(), "large-images.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	for _, part := range []string{
		`{"parentUuid":null,"type":"user","message":{"role":"user","content":"earlier user text"},"uuid":"a","timestamp":"2026-10-25T03:50:25.170Z"}` + "\n",
		`{"parentUuid":"a","type":"user","message":{"role":"user","content":[{"type":"text","text":"images supplied"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`,
		large,
		`"}},{"type":"text","text":"between images"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"`,
		small,
		`"}},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"`,
		small,
		`"}},{"type":"text","text":"after large images"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`,
		tiny,
		`"}}]},"uuid":"b","timestamp":"2026-10-25T03:50:26.170Z"}` + "\n",
		`{"parentUuid":"b","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"later assistant text"}]},"uuid":"c","timestamp":"2026-10-25T03:50:27.170Z"}` + "\n",
	} {
		if _, err := io.WriteString(w, part); err != nil {
			f.Close()
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	s := Session{Agent: "claude", ID: "large-images", Cwd: carryCwd, Path: path}
	r, err := CarryOver(s, "omp")
	if err != nil {
		t.Fatal(err)
	}
	checkOmpCarryJournal(t, r, carryCwd)
	lines := readLines(t, r.Path)
	message := lines[3]["message"].(map[string]any)
	blocks := message["content"].([]any)
	if len(blocks) != 7 || blocks[2].(map[string]any)["text"] != "between images" || blocks[5].(map[string]any)["text"] != "after large images" {
		t.Fatalf("image message content: %v", blocks)
	}
	for i, want := range []string{"blob:sha256:" + largeHash, "blob:sha256:" + smallHash, "blob:sha256:" + smallHash, tiny} {
		image := blocks[[]int{1, 3, 4, 6}[i]].(map[string]any)
		if image["type"] != "image" || image["data"] != want {
			t.Fatalf("image %d: %v", i, image)
		}
	}
	for hash, want := range map[string][]byte{largeHash: largeBytes, smallHash: smallBytes} {
		got, err := os.ReadFile(filepath.Join(blobs, hash))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("blob %s: %d bytes, %v", hash, len(got), err)
		}
	}
	tr, err := TranscriptOf(Session{Agent: "omp", Path: r.Path})
	if err != nil {
		t.Fatal(err)
	}
	text := transcriptText(t, tr)
	for _, want := range []string{"earlier user text", "images supplied", "[image]", "later assistant text"} {
		if !strings.Contains(text, want) {
			t.Fatalf("large-image branch missing %q: %s", want, text)
		}
	}

	before, err := filepath.Glob(filepath.Join(OmpDir(), "sessions", "*", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	invalid := []carryItem{{Kind: "user", Content: []carryContent{{Type: "text", Text: "bad image"}, {Type: "image", Data: strings.Repeat("!", 1024), MimeType: "image/png"}}}}
	if _, err := writeOmpCarry(carryCwd, invalid, time.Now()); err == nil {
		t.Fatal("invalid large image was silently dropped or mangled")
	}
	after, err := filepath.Glob(filepath.Join(OmpDir(), "sessions", "*", "*.jsonl"))
	if err != nil || len(after) != len(before) {
		t.Fatalf("invalid image published a journal: before %v, after %v, error %v", before, after, err)
	}
}

func TestCarryOmpReusesBlobWithWriteGrace(t *testing.T) {
	carrySetup(t)
	data := bytes.Repeat([]byte{0x61}, 768)
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	path := filepath.Join(OmpDir(), "blobs", hash)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	items := []carryItem{{Kind: "user", Content: []carryContent{{Type: "text", Text: "image"}, {Type: "image", Data: base64.StdEncoding.EncodeToString(data), MimeType: "image/png"}}}}
	r, err := writeOmpCarry(carryCwd, items, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	message := readLines(t, r.Path)[2]["message"].(map[string]any)
	image := message["content"].([]any)[1].(map[string]any)
	if image["data"] != "blob:sha256:"+hash {
		t.Fatalf("reused image reference: %v", image)
	}
	st, err := os.Stat(path)
	if err != nil || !st.ModTime().After(old) {
		t.Fatalf("reused blob did not get write grace: %v, %v", st, err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("reused blob bytes: %d, %v", len(got), err)
	}
}

func TestCarryOmpTimestampFallbackAndResultLimit(t *testing.T) {
	carrySetup(t)
	now := time.Date(2026, 10, 25, 3, 50, 25, 123456789, time.UTC)
	items := pairCarry([]carryItem{
		{Kind: "user", Content: []carryContent{{Type: "text", Text: "run it"}}},
		{Kind: "call", ID: "one", Name: "Bash", Shell: "show"},
		{Kind: "result", ID: "one", Text: strings.Repeat("a", carryResultMax-1) + "é" + "tail"},
	})
	r, err := writeOmpCarry(carryCwd, items, now)
	if err != nil {
		t.Fatal(err)
	}
	calls := checkOmpCarryJournal(t, r, carryCwd)
	if len(calls) != 1 || calls[0].output != strings.Repeat("a", carryResultMax-1)+"\n[…]" {
		t.Fatalf("truncated shell result: %+v", calls)
	}
	for _, entry := range readLines(t, r.Path)[2:] {
		message := entry["message"].(map[string]any)
		if message["timestamp"] != float64(now.UnixMilli()) {
			t.Fatalf("missing timestamp did not use current time: %v", message)
		}
	}
}

func TestCarryOmpBucketBoundaries(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	temp := filepath.Join(home, "scratch")
	for _, dir := range []string{home, temp, filepath.Join(home, "project", "src"), filepath.Join(temp, "project"), filepath.Join(home, "scratch-other"), filepath.Join(home, "..hidden"), home + "-other"} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	canonical, err := filepath.EvalSymlinks(home + "-other")
	if err != nil {
		t.Fatal(err)
	}
	dotdot, err := filepath.EvalSymlinks(filepath.Join(home, "..hidden"))
	if err != nil {
		t.Fatal(err)
	}
	absoluteName := func(path string) string {
		if strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`) {
			path = path[1:]
		}
		return "--" + strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(path) + "--"
	}
	for _, tc := range []struct{ cwd, want string }{
		{home, "-"},
		{filepath.Join(home, "project", "src"), "-project-src"},
		{temp, "-tmp"},
		{filepath.Join(temp, "project"), "-tmp-project"},
		{filepath.Join(home, "scratch-other"), "-scratch-other"},
		{filepath.Join(home, "..hidden"), absoluteName(dotdot)},
		{home + "-other", absoluteName(canonical)},
	} {
		if got := ompCarryBucket(tc.cwd, home, temp); got != tc.want {
			t.Errorf("bucket(%q) = %q, want %q", tc.cwd, got, tc.want)
		}
	}
	if runtime.GOOS != "windows" {
		special := filepath.Join(home, `colon:and\slash`)
		if err := os.MkdirAll(special, 0o700); err != nil {
			t.Fatal(err)
		}
		if got := ompCarryBucket(special, home, temp); got != "-colon-and-slash" {
			t.Errorf("encoded bucket = %q", got)
		}
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(home, alias); err == nil {
		if got := ompCarryBucket(filepath.Join(alias, "scratch", "project"), home, temp); got != "-tmp-project" {
			t.Errorf("symlinked cwd bucket = %q", got)
		}
		if got := ompCarryBucket(filepath.Join(home, "project", "src"), alias, temp); got != "-project-src" {
			t.Errorf("symlinked home bucket = %q", got)
		}
		tempAlias := filepath.Join(root, "temp-alias")
		if err := os.Symlink(temp, tempAlias); err != nil {
			t.Fatal(err)
		}
		if got := ompCarryBucket(filepath.Join(temp, "project"), home, tempAlias); got != "-tmp-project" {
			t.Errorf("symlinked temp bucket = %q", got)
		}
		missing := filepath.Join(alias, "missing")
		if got := ompCarryBucket(missing, home, temp); got != absoluteName(missing) {
			t.Errorf("missing cwd bucket = %q, want %q", got, absoluteName(missing))
		}
	} else if runtime.GOOS != "windows" {
		t.Fatalf("creating symlink: %v", err)
	}
}

func TestCarryOmpDetection(t *testing.T) {
	setup(t)
	thereMu.Lock()
	old := thereAt
	thereAt = map[string]there{}
	thereMu.Unlock()
	t.Cleanup(func() {
		thereMu.Lock()
		thereAt = old
		thereMu.Unlock()
	})
	clear := func() {
		thereMu.Lock()
		thereAt = map[string]there{}
		thereMu.Unlock()
	}
	empty, bin := t.TempDir(), t.TempDir()
	t.Setenv("PATH", empty)
	if agentThere("omp") {
		t.Fatal("omp detected with no command or folder")
	}
	name := "omp"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte("fake executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	clear()
	if !agentThere("omp") {
		t.Fatal("omp command not detected")
	}
	if err := os.Remove(filepath.Join(bin, name)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", empty)
	xdg := filepath.Join(t.TempDir(), "data")
	t.Setenv("XDG_DATA_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "omp"), 0o700); err != nil {
		t.Fatal(err)
	}
	clear()
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if !agentThere("omp") {
			t.Fatal("initialized XDG omp folder not detected")
		}
		offered := carries(Session{Agent: "codex", Cwd: "/w", Path: "/w/x.jsonl"})
		found := false
		for _, offer := range offered {
			if offer == (Carry{Agent: "omp", Convert: true}) {
				found = true
			}
		}
		if !found {
			t.Fatalf("initialized XDG omp not offered: %+v", offered)
		}
	} else if agentThere("omp") {
		t.Fatal("XDG omp detected on an unsupported platform")
	}
	if err := os.Remove(filepath.Join(xdg, "omp")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(OmpDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	clear()
	if !agentThere("omp") {
		t.Fatal("omp folder not detected")
	}
}
