package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// startSizes is how a script harness's run logs the size of each of its
// arguments and environment variables as it starts.
const startSizes = `for a in "$@"; do echo "arglen ${#a}" >> $log; done
awk 'BEGIN { for (k in ENVIRON) print "envlen " (length(k) + 1 + length(ENVIRON[k])) }' >> $log`

// bigSafeguards and bigSchema are each over 128KiB, what Linux starts a
// program with in one argument or variable.
var (
	bigContext    = strings.Repeat("The user allows edits in the project. ", 4000)
	bigSafeguards = `"safeguards":[{"type":"dangerous_tool_use","classifier_context":"` + bigContext + `"}],`
	bigSchema     = func() string {
		props := make([]string, 0, 3000)
		for i := range 3000 {
			props = append(props, `"field_`+strconv.Itoa(i)+`_of_the_answer":{"type":"string","description":"one of the answer's many fields"}`)
		}
		return `{"type":"object","properties":{` + strings.Join(props, ",") + `}}`
	}()
)

// askRaw sends a turn of body's and returns the status, whatever it is.
func (h *resumeHarness) askRaw(body string) int {
	h.t.Helper()
	rec := httptest.NewRecorder()
	var u Usage
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	code, _ := h.s.serveClaudeSubscription(rec, r, provider.Anthropic, h.p, "claude-sonnet-5", []byte(body), &u)
	return code
}

// startBound checks each run's arguments and environment: no value over
// startValueMax that the run wasn't started with before this change (the
// test's own environment), and no more than 64KiB of them in all.
func startBound(t *testing.T, runs map[string]string) {
	t.Helper()
	if len(runs) == 0 {
		t.Fatal("no run started")
	}
	base := 0
	for _, kv := range os.Environ() {
		base = max(base, len(kv))
	}
	for name, log := range runs {
		total := 0
		sc := bufio.NewScanner(strings.NewReader(log))
		sc.Buffer(nil, 4<<20)
		for sc.Scan() {
			kind, n, ok := strings.Cut(sc.Text(), " ")
			if kind != "arglen" && kind != "envlen" || !ok {
				continue
			}
			size, _ := strconv.Atoi(n)
			if size > startValueMax && size > base {
				t.Fatalf("run %s was started with a %s of %d bytes, over %d", name, strings.TrimSuffix(kind, "len"), size, startValueMax)
			}
			if kind == "arglen" {
				total += size
			}
		}
		if total > 64<<10 {
			t.Fatalf("run %s was started with %d bytes of arguments", name, total)
		}
	}
}

// #6848: a long conversation's safeguards and a large JSON schema went on
// a run's command line and in its environment whole, and Linux refused
// to start it, "argument list too long", the resume and the replay alike:
// every request after was a 502. The interactive mode's run is given the
// safeguards in its guard file, a bounded stand-in in the variable, and
// no schema (its answer tool has it).
func TestClaudeTUIStartBounded(t *testing.T) {
	h := tuiScriptHarness(t, startSizes)
	h.extra = bigSafeguards
	h.ask("rules", "a1")
	startBound(t, h.runs())
	for _, log := range h.runs() {
		if !strings.Contains(log, "envlen ") {
			t.Fatalf("the run logged no sizes: %.500s", log)
		}
	}
	body := `{"model":"claude-sonnet-5","max_tokens":100,` + bigSafeguards + `"system":"rules","messages":[{"role":"user","content":"b1"}],"output_config":{"format":{"type":"json_schema","schema":` + bigSchema + `}}}`
	h.askRaw(body)
	startBound(t, h.runs())
	for _, log := range h.runs() {
		if strings.Contains(log, "--json-schema") {
			t.Fatalf("the interactive mode was given --json-schema: %.300s", log)
		}
	}
}

// -p is given safeguards over standard input (apply_flag_settings) and a
// schema too large for its command line with its initialize, as the
// Agent SDK hands one over.
func TestClaudePrintStartBounded(t *testing.T) {
	h := newScriptHarness(t, "ok", startSizes)
	body := `{"model":"claude-sonnet-5","max_tokens":100,` + bigSafeguards + `"system":"rules","messages":[{"role":"user","content":"a1"}],"output_config":{"format":{"type":"json_schema","schema":` + bigSchema + `}}}`
	h.askRaw(body)
	runs := h.runs()
	startBound(t, runs)
	seen := false
	for _, log := range runs {
		for _, l := range strings.Split(log, "\n") {
			if !strings.HasPrefix(l, "control ") || !strings.Contains(l, `"subtype":"initialize"`) {
				continue
			}
			var m struct {
				Request struct {
					JSONSchema json.RawMessage `json:"jsonSchema"`
				} `json:"request"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(l, "control ")), &m); err != nil {
				t.Fatal(err)
			}
			if string(m.Request.JSONSchema) == string(cliSchema(json.RawMessage(bigSchema))) {
				seen = true
			}
		}
		if strings.Contains(log, "--json-schema") {
			t.Fatalf("a schema too large for the command line was put there: %.300s", log)
		}
		if !strings.Contains(log, bigContext[:200]) || !strings.Contains(log, "apply_flag_settings") {
			t.Fatalf("the run wasn't told the safeguards: %.300s", log)
		}
	}
	if !seen {
		t.Fatal("no run's initialize has the schema")
	}
}

func TestClaudePrintStartInterruptsBlockedInitialize(t *testing.T) {
	for _, useCancel := range []bool{false, true} {
		name := "deadline"
		if useCancel {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			h := newScriptHarness(t, "unused", `
dd bs=1 count=1 >/dev/null 2>/dev/null || exit 17
echo 'read one byte' >> "$log"
exec sleep 30
`)
			oldWait := controlWait
			controlWait = time.Second
			if useCancel {
				controlWait = 10 * time.Second
			}
			t.Cleanup(func() { controlWait = oldWait })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			runReady := make(chan *subscriptionRun, 1)
			done := make(chan error, 1)
			schema := `{"type":"object","description":"` + strings.Repeat("s", 2<<20) + `"}`
			req := &Request{
				Messages: []Message{{Role: "user", Parts: []Part{{Kind: Text, Text: "hi"}}}},
				Format:   &Format{Type: "json_schema", Schema: json.RawMessage(schema)},
				runTold:  func(run *subscriptionRun) { runReady <- run },
			}
			go func() {
				_, _, err := h.s.subscription.start(ctx, req, "claude-sonnet-5", "", "", nil)
				done <- err
				close(done)
			}()
			var run *subscriptionRun
			t.Cleanup(func() {
				cancel()
				if run != nil {
					select {
					case <-run.abort():
					case <-time.After(6 * time.Second):
						t.Error("the Claude stand-in did not stop")
					}
				}
				select {
				case <-done:
				case <-time.After(6 * time.Second):
					t.Error("the start caller did not return after cleanup")
				}
			})
			select {
			case run = <-runReady:
			case err := <-done:
				t.Fatalf("the run did not reach initialize: %v", err)
			case <-time.After(6 * time.Second):
				t.Fatal("the run was not registered")
			}
			poll := time.NewTicker(10 * time.Millisecond)
			defer poll.Stop()
			limit := time.After(3 * time.Second)
			read := false
			for !read {
				select {
				case <-poll.C:
					for _, log := range h.runs() {
						read = read || strings.Contains(log, "read one byte")
					}
				case <-limit:
					t.Fatal("the Claude stand-in did not begin reading the initialize frame")
				}
			}
			if useCancel {
				cancel()
			}
			select {
			case err := <-done:
				if useCancel {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("initialize after cancellation: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "answered magpie's initialize request in 1s") {
					t.Fatalf("initialize after its deadline: %v", err)
				}
				if _, err := run.stdin.Write([]byte("another frame\n")); err == nil {
					t.Fatal("the interrupted stdin still accepts frames")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("initialize stayed blocked in the subprocess stdin write")
			}
		})
	}
}

// A schema that fits goes on -p's command line as before, where Claude
// Code exits over one it can't use (#3036); a large one it can't use is
// a 400 before a run starts, as an initialize's is passed over.
func TestClaudePrintSchemaPlaces(t *testing.T) {
	h := newScriptHarness(t, "ok", "")
	small := `{"type":"object","properties":{"a":{"type":"string"}}}`
	h.askRaw(`{"model":"claude-sonnet-5","max_tokens":100,"system":"rules","messages":[{"role":"user","content":"a1"}],"output_config":{"format":{"type":"json_schema","schema":` + small + `}}}`)
	got := false
	for _, log := range h.runs() {
		got = got || strings.Contains(log, "--json-schema "+small)
	}
	if !got {
		t.Fatalf("a small schema wasn't on the command line: %v", h.runs())
	}
	bad := strings.Replace(bigSchema, `"type":"string"`, `"type":"strng"`, 1)
	before := len(h.runs())
	if code := h.askRaw(`{"model":"claude-sonnet-5","max_tokens":100,"system":"rules","messages":[{"role":"user","content":"b1"}],"output_config":{"format":{"type":"json_schema","schema":` + bad + `}}}`); code != 400 {
		t.Fatalf("a large schema Claude Code can't use: %d", code)
	}
	if len(h.runs()) != before {
		t.Fatal("a run started for a schema refused")
	}
}
