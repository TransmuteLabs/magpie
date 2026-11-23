package gateway

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/usage"
)

// Copilot's /responses gives an item a new id in every event (the fixture
// is a real reply, gpt-5.5 offered web_search and a function, its
// encrypted parts cut short). Relayed to Codex, each item keeps the one
// id it was added with, in its events, its done and response.completed,
// so Codex shows the commentary once and ends the search it started
// (echo_ts on Discord).
func TestCopilotItemKeepsItsID(t *testing.T) {
	reply, err := os.ReadFile("testdata/copilot_rotating_ids.sse")
	if err != nil {
		t.Fatal(err)
	}
	c := copilotSol(t)
	c.mu.Lock()
	c.reply = string(reply)
	c.mu.Unlock()
	code, out := postTo(t, New(), "/v1/responses", `{"model":"copilot/gpt-6-luna","stream":true,"probe":"as sent",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Search for the latest Go release, then run go version."}]}],
		"tools":[{"type":"function","name":"exec_command","description":"Runs a command","strict":false,"parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},{"type":"web_search"}]}`)
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if c.last(t)["probe"] != "as sent" {
		t.Fatal("not relayed: the reply under test is Copilot's own")
	}
	ids := map[int64]map[string]bool{} // output_index → every id it was given
	note := func(at int64, id string) {
		if ids[at] == nil {
			ids[at] = map[string]bool{}
		}
		ids[at][id] = true
	}
	texts := map[string]int{}
	completed := false
	for _, line := range strings.Split(out, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		e := gjson.Parse(data)
		switch e.Get("type").String() {
		case "response.completed":
			completed = true
			for i, it := range e.Get("response.output").Array() {
				note(int64(i), it.Get("id").String())
			}
			continue
		case "response.output_item.done":
			if it := e.Get("item"); it.Get("type").String() == "message" {
				texts[it.Get("content.0.text").String()]++
			}
		}
		at := e.Get("output_index")
		if !at.Exists() {
			continue
		}
		if id := e.Get("item.id"); id.Exists() {
			note(at.Int(), id.String())
		}
		if id := e.Get("item_id"); id.Exists() {
			note(at.Int(), id.String())
		}
	}
	if !completed || len(ids) != 4 {
		t.Fatalf("completed %v, items %d: %s", completed, len(ids), out)
	}
	for at, seen := range ids {
		if len(seen) != 1 {
			t.Errorf("item %d went by %d ids", at, len(seen))
		}
	}
	if len(texts) != 1 {
		t.Errorf("messages done: %v", texts)
	}
}

// An upstream whose ids agree, as OpenAI's do, goes through byte for byte.
func TestItemIDsThatAgreeGoAsTheyCame(t *testing.T) {
	stream := sse(
		`data: {"type":"response.created","response":{"id":"r1"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`,
		`data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","content_index":0,"delta":"a <b> & c"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"a <b> & c"}]}}`,
		`data: {"type":"response.completed","response":{"id":"r1","output":[{"type":"message","id":"msg_1"}],"usage":{"input_tokens":1.0}}}`,
	)
	var got []byte
	tidy := &idTidy{}
	for i := 0; i < len(stream); i += 7 {
		got = append(got, tidy.write([]byte(stream[i:min(i+7, len(stream))]))...)
	}
	got = append(got, tidy.flush()...)
	if string(got) != stream {
		t.Errorf("changed:\n%s\nwant:\n%s", got, stream)
	}
}

func TestIDTidyFirstTrackedEvent(t *testing.T) {
	const fields = `,"output_index":0,"item":{"id":"first","type":"message"}}`
	const added = `{"type":"response.output_item.added"` + fields
	const delta = `data: {"type":"response.output_text.delta","output_index":0,"item_id":"later","delta":"hi"}` + "\n\n"
	for _, test := range []struct {
		name string
		data string
		seed bool
	}{
		{"object", " \t" + added, true},
		{"escaped_key_and_value", `{"\u0074ype":"response.output_item.\u0061dded"` + fields, true},
		{"first_duplicate_added", `{"type":"response.output_item.added","type":"response.created"` + fields, true},
		{"first_duplicate_other", `{"type":"response.created","ty\u0070e":"response.output_item.added"` + fields, false},
		{"first_duplicate_null", `{"type":null,"type":"response.output_item.added"` + fields, false},
		{"array_root", `[` + added + `]`, false},
		{"string_root", `"{\"type\":\"response.output_item.added\"}"`, false},
		{"nested_type", `{"payload":` + added + `}`, false},
		{"invalid_after_type", added + ` trailing`, false},
		{"truncated_after_type", strings.TrimSuffix(added, "}"), false},
		{"missing_index", `{"type":"response.output_item.added","item":{"id":"first"}}`, false},
		{"missing_id", `{"type":"response.output_item.added","output_index":0,"item":{}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			tidy := &idTidy{}
			first := []byte("data: " + test.data + "\n\n")
			if got := tidy.event(first); !bytes.Equal(got, first) || (len(tidy.ids) != 0) != test.seed {
				t.Fatalf("first event %q and tracked IDs %v", got, tidy.ids)
			}
			next := tidy.event([]byte(delta))
			if test.seed {
				if got := gjson.GetBytes(eventData(next), "item_id").Str; got != "first" {
					t.Fatalf("rewritten item ID %q", got)
				}
			} else if string(next) != delta {
				t.Fatalf("untracked delta changed: %q", next)
			}
		})
	}
}

func TestUntrackedItemIDsOverResponses(t *testing.T) {
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_tidy","object":"response","status":"in_progress","output":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"item_id":"delta","content_index":0,"delta":"hi"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"done","type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"hi"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_tidy","object":"response","status":"completed","output":[{"id":"ended","type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}`,
	}
	lines := make([]string, len(frames))
	for index, frame := range frames {
		lines[index] = "data: " + frame
	}
	stream := sse(lines...)
	for _, transport := range []string{"sse", "ws"} {
		t.Run(transport, func(t *testing.T) {
			fresh(t)
			up := &respUp{reply: func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, stream)
			}}
			sockUp(t, up)
			_, front := sockServer(t)
			if transport == "sse" {
				reply, err := front.Client().Post(front.URL+"/v1/responses", "application/json",
					strings.NewReader(`{"model":"fake/m1","stream":true,"store":false,"input":[`+userHi+`]}`))
				if err != nil {
					t.Fatal(err)
				}
				defer reply.Body.Close()
				body, err := io.ReadAll(reply.Body)
				if err != nil || reply.StatusCode != http.StatusOK || string(body) != stream {
					t.Fatalf("SSE status %d, read %v, wire %q", reply.StatusCode, err, body)
				}
			} else {
				client, _, err := dialSock(t, front, "/v1/responses", nil)
				if err != nil {
					t.Fatal(err)
				}
				events := client.turn(create("", userHi))
				if len(events) != len(frames) {
					t.Fatalf("got %d events, want %d", len(events), len(frames))
				}
				for index, event := range events {
					if event.Raw != frames[index] {
						t.Fatalf("frame %d changed: %q", index, event.Raw)
					}
				}
			}
			waitFor(t, "persisted Responses usage", func() bool { return len(usage.Load(time.Time{})) != 0 })
			records := usage.Load(time.Time{})
			if up.calls() != 1 || len(records) != 1 || records[0].Provider != "fake" ||
				records[0].Input != 7 || records[0].Output != 2 {
				t.Fatalf("upstream calls %d, usage %v", up.calls(), records)
			}
		})
	}
}
