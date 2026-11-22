package gateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"weak"

	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
)

func TestResponsesSocketFreesQueuedFrameWhileOpen(t *testing.T) {
	fresh(t)
	const marker = "queued raw frame:"
	frame := strings.Replace(create("resp_probe_1", userMore), "be brief", marker+strings.Repeat("x", 256<<10), 1)
	frameSize := len(frame)
	observed := make(chan weak.Pointer[byte], 1)
	var captured atomic.Bool
	capture := func(data []byte) {
		if len(data) == frameSize && gjson.GetBytes(data, "type").String() == "response.create" &&
			strings.HasPrefix(gjson.GetBytes(data, "instructions").String(), marker) && captured.CompareAndSwap(false, true) {
			observed <- weak.Make(&data[0])
		}
	}
	prior := validHashed.Swap(&capture)
	t.Cleanup(func() { validHashed.Store(prior) })

	first, third := make(chan struct{}), make(chan struct{})
	releaseFirst, releaseThird := sync.OnceFunc(func() { close(first) }), sync.OnceFunc(func() { close(third) })
	history := make(chan [5]string, 1)
	var calls atomic.Int64
	key := rand.Text()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusMisdirectedRequest)
			return
		}
		attempt := calls.Add(1)
		var release <-chan struct{}
		if attempt == 3 {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				history <- [5]string{}
				return
			}
			var text [5]string
			for index := range text {
				text[index] = gjson.GetBytes(body, fmt.Sprintf("input.%d.content.0.text", index)).String()
			}
			if gjson.GetBytes(body, "input.#").Int() != int64(len(text)) {
				text = [5]string{}
			}
			history <- text
			release = third
		} else {
			_, _ = io.Copy(io.Discard, r.Body)
			if attempt == 1 {
				release = first
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		id := fmt.Sprintf("resp_probe_%d", attempt)
		io.WriteString(w, sse(
			`data: {"type":"response.created","response":{"id":"`+id+`","object":"response","status":"in_progress","output":[]}}`,
			`data: {"type":"response.output_text.delta","item_id":"msg_probe_`+fmt.Sprint(attempt)+`","output_index":0,"content_index":0,"delta":"answer"}`))
		w.(http.Flusher).Flush()
		if release != nil {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		item := fmt.Sprintf(`{"type":"message","id":"msg_probe_%d","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer %d"}]}`, attempt, attempt)
		io.WriteString(w, sse(`data: {"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
			`data: {"type":"response.completed","response":{"id":"`+id+`","object":"response","status":"completed","output":[`+item+`],"usage":{"input_tokens":300,"output_tokens":50,"total_tokens":350}}}`))
	}))
	t.Cleanup(up.Close)
	defer releaseFirst()
	defer releaseThird()
	if err := provider.Save(provider.Provider{ID: "fake", Name: "Fake", Key: key, Responses: up.URL + "/v1", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}

	_, server := sockServer(t)
	client, _, err := dialSock(t, server, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	client.send(create("", userHi))
	if events, err := client.until("response.created", "error"); err != nil || last(events).Get("response.id").String() != "resp_probe_1" {
		t.Fatalf("first turn did not start: %v", err)
	}
	client.send(frame)
	client.send(`{"type":"response.interrupt","response_id":"other"}`)
	if events, err := client.until("response.interrupt.failed", "error"); err != nil || last(events).Get("type").String() != "response.interrupt.failed" {
		t.Fatalf("queued frame barrier: %v", err)
	}
	releaseFirst()
	for _, id := range []string{"resp_probe_1", "resp_probe_2"} {
		if events, err := client.until("response.completed", "error"); err != nil || last(events).Get("response.id").String() != id {
			t.Fatalf("turn %s did not complete: %v", id, err)
		}
	}
	client.send(create("resp_probe_2", strings.Replace(userHi, `"hi"`, `"continuation"`, 1)))
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case got := <-history:
		want := [5]string{"hi", "answer 1", "and more", "answer 2", "continuation"}
		if got != want {
			t.Fatalf("continuation history = %q, want %q", got, want)
		}
	case <-timer.C:
		t.Fatal("continuation did not reach the vendor")
	}
	select {
	case raw := <-observed:
		runtime.GC()
		if raw.Value() != nil {
			t.Fatal("the drained queued frame is still retained while its socket is open")
		}
	default:
		t.Fatal("the queued frame's backing allocation was not observed")
	}
	releaseThird()
	if events, err := client.until("response.completed", "error"); err != nil || last(events).Get("response.id").String() != "resp_probe_3" {
		t.Fatalf("same-socket continuation did not complete: %v", err)
	}
}

// A socket turn's body is freed when the turn and its bounded prompt
// cache entry are gone (casyubaji on Discord: ~2.8 GB after 6 hours of
// Codex). Each turn is served from a body built in memory, the whole
// conversation, and the trace keeps its
// last 60 routes: a route that kept the turn's request context (its tool
// scan, for the turn's end) kept the request on it (the inflight's
// unrouted watch), and with it the body, so 60 of Codex's 20 MB
// conversations stayed after every socket had closed.
func TestSocketTurnBodiesFreed(t *testing.T) {
	t.Run("upstream HTTP", func(t *testing.T) { expectSocketTurnBodiesFreed(t, "") })
	t.Run("upstream WebSocket", func(t *testing.T) { expectSocketTurnBodiesFreed(t, provider.SocketOn) })
}

func expectSocketTurnBodiesFreed(t *testing.T, socket string) {
	t.Helper()
	fresh(t)
	var served atomic.Int64
	var vendor http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		n := served.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		id := fmt.Sprintf("resp_up%d", n)
		item := fmt.Sprintf(`{"type":"message","id":"msg_%d","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer %d"}]}`, n, n)
		io.WriteString(w, sse(
			`data: {"type":"response.created","response":{"id":"`+id+`","object":"response","status":"in_progress","output":[]}}`,
			`data: {"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
			`data: {"type":"response.completed","response":{"id":"`+id+`","object":"response","status":"completed","output":[`+item+`],"usage":{"input_tokens":300,"output_tokens":50,"total_tokens":350}}}`))
	})
	var sockets *fakeSocketBackend
	if socket == provider.SocketOn {
		socketOn(t)
		sockets = &fakeSocketBackend{path: "/v1/responses", keep: true}
		sockets.frames = keptReply(sockets)
		vendor = sockets
	}
	up := httptest.NewServer(withoutAnonymousFixtureProbe(vendor))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "fake", Name: "Fake", Key: "k", Responses: up.URL + "/v1", Models: []string{"m1"}, Socket: socket}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var bodies []weak.Pointer[byte]
	socketTurnBody = func(body []byte) {
		mu.Lock()
		bodies = append(bodies, weak.Make(&body[0]))
		mu.Unlock()
	}
	t.Cleanup(func() { socketTurnBody = nil })

	s, ts := sockServer(t)
	c, _, err := dialSock(t, ts, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	big := `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat("x", 256<<10) + `"}]}`
	const turns = 12
	id := ""
	for i := range turns {
		in := userMore
		if i == 0 {
			in = big
		}
		got := c.turn(create(id, in))
		end := last(got)
		if end.Get("type").String() != "response.completed" {
			t.Fatalf("turn %d: %v", i, got)
		}
		id = end.Get("response.id").String()
	}
	if sockets != nil {
		if posts, observed := sockets.counts(); posts != 0 || observed != turns {
			t.Fatalf("upstream got %d WS turns and %d HTTP posts, want %d WS turns", observed, posts, turns)
		}
	}
	c.c.CloseNow()
	// the socket's handler ends after its client goes (drain waits for
	// it); nothing of a turn is kept after, but the trace's routes. A
	// held body is let go by a context.AfterFunc (holdBody, body room),
	// which runs in a goroutine of its own once the turn's context ends,
	// so under load a collection can come before it: collect until each
	// is gone, within a bar far under what a real holder keeps one for
	// (#5250: a stopped timer kept the last turn's request for up to
	// unroutedMost, 2m; unroutedShow is 15s).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.sockets.drain(ctx)
	if ctx.Err() != nil {
		t.Fatal("the socket's handler did not end after its client went")
	}
	s.trace.mu.Lock()
	s.trace.bodies = nil
	s.trace.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != turns {
		t.Fatalf("%d turn bodies for %d turns", len(bodies), turns)
	}
	var kept []int
	for bar := time.Now().Add(10 * time.Second); ; {
		runtime.GC()
		kept = kept[:0]
		for i, b := range bodies {
			if b.Value() != nil {
				kept = append(kept, i)
			}
		}
		if len(kept) == 0 || time.Now().After(bar) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(kept) > 0 {
		t.Fatalf("turn bodies %v of %d still kept after the socket's handler ended", kept, turns)
	}
}

func TestResponsesSocketReusesTurnBody(t *testing.T) {
	t.Run("unrestricted", func(t *testing.T) { expectResponsesSocketReusesTurnBody(t, false) })
	t.Run("model budget", func(t *testing.T) { expectResponsesSocketReusesTurnBody(t, true) })
}

func expectResponsesSocketReusesTurnBody(t *testing.T, modelBudget bool) {
	t.Helper()
	fresh(t)
	built := make(chan []byte, 1)
	socketTurnBody = func(body []byte) { built <- body }
	t.Cleanup(func() { socketTurnBody = nil })

	reused := make(chan bool, 1)
	up := &respUp{reply: func(w http.ResponseWriter, r *http.Request, _ int) {
		select {
		case body := <-built:
			reused <- sharesBodyListed(body)
		default:
			http.Error(w, "no socket turn body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`data: {"type":"response.completed","response":{"id":"resp_reused","status":"completed","output":[]}}`))
	}}
	upstream := httptest.NewServer(withoutAnonymousFixtureProbe(up))
	t.Cleanup(upstream.Close)
	if err := provider.Save(provider.Provider{
		ID: "fake", Name: "Fake", Key: "k", Responses: upstream.URL + "/v1", Models: []string{"m1"},
	}); err != nil {
		t.Fatal(err)
	}
	var headers http.Header
	if modelBudget {
		keys, secrets := newCaller(t, "Budgeted")
		setLimit(t, keys[0].ID, &access.Limit{Period: "day", Tokens: 1 << 30, Models: []string{"fake/m1"}})
		headers = http.Header{"Authorization": {"Bearer " + secrets[0]}, "X-Test-Remote": {"192.168.1.9:5000"}}
	}
	_, server := sockServer(t)
	client, _, err := dialSock(t, server, "/v1/responses", headers)
	if err != nil {
		t.Fatal(err)
	}
	item := strings.Replace(userPing, "ping", strings.Repeat("x", 256<<10), 1)
	frame := create("", item)
	want := []string{item}
	if modelBudget {
		warmup := last(client.turn(create("", item, `"generate":false`)))
		if warmup.Get("type").String() != "response.completed" || up.calls() != 0 {
			t.Fatalf("warmup ended as %s with %d upstream calls", warmup.Raw, up.calls())
		}
		frame = create(warmup.Get("response.id").String(), userMore)
		want = append(want, userMore)
	}
	if end := last(client.turn(frame)); end.Get("type").String() != "response.completed" {
		t.Fatalf("turn ended as %s", end.Raw)
	}
	if !<-reused {
		t.Fatal("gateway routed a copy of the socket's POST body")
	}
	if up.calls() != 1 {
		t.Fatalf("upstream received %d requests for one continuation", up.calls())
	}
	input := gjson.GetBytes(up.body(0), "input").Array()
	if len(input) != len(want) {
		t.Fatalf("upstream received %d input items, want %d", len(input), len(want))
	}
	for index, item := range want {
		if input[index].Raw != item {
			t.Fatalf("upstream input item %d changed", index)
		}
	}
}

func TestResponsesSocketDoesNotRetainSpareBodyCapacity(t *testing.T) {
	fresh(t)
	built := make(chan []byte, 1)
	socketTurnBody = func(body []byte) { built <- body }
	t.Cleanup(func() { socketTurnBody = nil })
	up := &respUp{}
	sockUp(t, up)
	server, httpServer := sockServer(t)
	client, _, err := dialSock(t, httpServer, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	item := strings.Replace(userPing, `"type":"message"`, `"type":`+strings.Repeat(" ", 256<<10)+`"message"`, 1)
	item = strings.Replace(item, "ping", strings.Repeat("x", 64<<10), 1)
	if end := last(client.turn(create("", item))); end.Get("type").String() != "response.completed" {
		t.Fatalf("turn ended as %s", end.Raw)
	}
	body := <-built
	if cap(body) <= 2*len(body) {
		t.Fatalf("fixture allocated %d bytes for a %d-byte body", cap(body), len(body))
	}
	client.c.CloseNow()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	server.sockets.drain(ctx)
	if err := ctx.Err(); err != nil {
		t.Fatal(err)
	}
	server.trace.mu.Lock()
	defer server.trace.mu.Unlock()
	if len(server.trace.bodies) != 1 {
		t.Fatalf("trace retained %d prompt bodies", len(server.trace.bodies))
	}
	prompt := server.trace.bodies[0].body
	if cap(prompt) > 2*len(prompt) || &prompt[0] == &body[0] {
		t.Fatalf("trace retained %d bytes for a %d-byte prompt", cap(prompt), len(prompt))
	}
}

func TestResponsesSocketBoundsReconstructedBody(t *testing.T) {
	fresh(t)
	up := &respUp{}
	sockUp(t, up)
	server := New()
	server.requestLimits.body = 48 << 10
	client, _, err := dialSock(t, serveSock(t, server), "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	item := strings.Replace(userPing, "ping", strings.Repeat("x", 32<<10), 1)
	initial := last(client.turn(create("", item)))
	if initial.Get("type").String() != "response.completed" {
		t.Fatalf("initial turn ended as %s", initial.Raw)
	}
	previous := initial.Get("response.id").String()
	overflow := last(client.turn(create(previous, item)))
	if overflow.Get("type").String() != "error" || overflow.Get("status").Int() != http.StatusRequestEntityTooLarge {
		t.Fatalf("overflow turn ended as %s", overflow.Raw)
	}
	if calls := up.calls(); calls != 1 {
		t.Fatalf("upstream received %d turns after an oversized continuation", calls)
	}
	retry := last(client.turn(create(previous, userMore)))
	if retry.Get("type").String() != "response.completed" {
		t.Fatalf("shorter continuation ended as %s", retry.Raw)
	}
}

func TestResponsesSocketWarmupBoundsHeldHistory(t *testing.T) {
	const limit = 48 << 10
	const warmup = `"generate":false`
	longText := strings.Repeat("x", 32<<10)
	longItem := strings.Replace(userHi, "hi", longText, 1)
	htmlText := strings.Repeat("<", 9<<10)
	htmlItem := strings.Replace(userHi, "hi", htmlText, 1)
	pad := strings.Repeat(" ", 32<<10)
	padded := func(item string) string {
		return strings.Replace(item, `"type":`, `"type":`+pad, 1)
	}
	quotedItem := strings.Replace(userHi, "hi", ` \thi  \"quoted\" <&`+"\u2028", 1)
	edgeText := ` \t\"<>&\u003c` + "\u2028\u2029" + strings.Repeat("<", (8<<10)-128)
	edgeItem := func(text string) string {
		return padded(strings.Replace(userMore, "and more", text, 1))
	}
	encoded, err := json.Marshal([]json.RawMessage{json.RawMessage(userHi), json.RawMessage(edgeItem(edgeText))})
	if err != nil || len(encoded) > limit {
		t.Fatalf("boundary fixture base has %d bytes: %v", len(encoded), err)
	}
	exact := edgeText + strings.Repeat("x", limit-len(encoded))
	for _, route := range []struct {
		name, path string
	}{
		{name: "public", path: "/v1/responses"},
		{name: "native", path: CodexPath + "/responses"},
	} {
		t.Run(route.name, func(t *testing.T) {
			fresh(t)
			up := &respUp{}
			vendor := httptest.NewServer(withoutAnonymousFixtureProbe(up))
			t.Cleanup(vendor.Close)
			if err := provider.Save(provider.Provider{ID: "fake", Name: "Fake", Key: "k", Responses: vendor.URL + "/v1", Models: []string{"m1"}}); err != nil {
				t.Fatal(err)
			}
			server := New()
			server.requestLimits.body = limit
			listener := serveSock(t, server)
			for _, test := range []struct {
				name, first, second string
				history             []json.RawMessage
				tooLarge            bool
				warmupOnly          bool
			}{
				{name: "joined", first: longItem, second: longItem, history: []json.RawMessage{json.RawMessage(longItem)}, tooLarge: true},
				{name: "escaped", first: userHi, second: htmlItem, history: []json.RawMessage{json.RawMessage(userHi)}, tooLarge: true},
				{name: "exactly fits", first: userHi, second: edgeItem(exact), warmupOnly: true},
				{name: "one byte over", first: userHi, second: edgeItem(exact + "x"), history: []json.RawMessage{json.RawMessage(userHi)}, tooLarge: true},
				{name: "inter-token whitespace", first: padded(quotedItem), second: padded(userMore),
					history: []json.RawMessage{json.RawMessage(quotedItem), json.RawMessage(userMore)}},
			} {
				t.Run(test.name, func(t *testing.T) {
					client, _, err := dialSock(t, listener, route.path, nil)
					if err != nil {
						t.Fatal(err)
					}
					calls := up.calls()
					expectLocal := func(got []gjson.Result) string {
						t.Helper()
						out := last(got)
						id := out.Get("response.id").String()
						if len(got) != 2 || got[0].Get("type").String() != "response.created" ||
							out.Get("type").String() != "response.completed" || id == "" ||
							got[0].Get("response.id").String() != id {
							t.Fatalf("warmup ended with %s (%d events)", out.Raw, len(got))
						}
						return id
					}
					previous := expectLocal(client.turn(create("", test.first, warmup)))
					expectLocal(client.turn(create("", userHi, warmup)))
					if test.tooLarge {
						missing := last(client.turn(create("resp_missing", test.second, warmup)))
						if missing.Get("status").Int() != http.StatusBadRequest ||
							missing.Get("error.code").String() != "previous_response_not_found" {
							t.Errorf("unknown predecessor got %s", missing.Raw)
						}
					}
					got := client.turn(create(previous, test.second, warmup))
					if test.tooLarge {
						out := last(got)
						if len(got) != 1 || out.Get("type").String() != "error" ||
							out.Get("status").Int() != http.StatusRequestEntityTooLarge ||
							out.Get("error.type").String() != "request_too_large" {
							t.Errorf("oversized warmup ended with %s (%d events)", out.Raw, len(got))
						}
					} else {
						previous = expectLocal(got)
					}
					if n := up.calls() - calls; n != 0 {
						t.Fatalf("warmups sent %d upstream requests", n)
					}
					if test.warmupOnly {
						return
					}
					out := last(client.turn(create(previous, userPing)))
					if n := up.calls() - calls; out.Get("type").String() != "response.completed" || n != 1 {
						t.Fatalf("ordinary continuation ended with %s and %d upstream requests", out.Raw, n)
					}
					input := gjson.GetBytes(up.body(calls), "input").Raw
					want, err := json.Marshal(append(slices.Clone(test.history), json.RawMessage(userPing)))
					if err != nil {
						t.Fatal(err)
					}
					if input != string(want) {
						t.Errorf("upstream input differs (length %d, want %d)", len(input), len(want))
					}
				})
			}
		})
	}
}

func TestSocketOutputItemOwnsOnlyTheDecodedItem(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	item := `{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.txt\"}"}`
	for _, test := range []struct {
		name  string
		prior string
	}{
		{name: "one item"},
		{name: "duplicate item field", prior: `"item":"` + strings.Repeat("x", 32<<10) + `",`},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := []byte(`{"type":"response.output_item.done",` + test.prior + `"item":` + item + `}`)
			writer := socketTurnWriter{ctx: ctx}
			if err := writer.event(event); !errors.Is(err, context.Canceled) {
				t.Fatalf("send result = %v", err)
			}
			clear(event)
			if len(writer.items) != 1 || string(writer.items[0]) != item {
				t.Fatalf("retained items = %s, want %s", writer.items, item)
			}
			if got := cap(writer.items[0]); got > 2*len(item) {
				t.Errorf("%d retained bytes for a %d-byte item", got, len(item))
			}
		})
	}
}

func TestResponsesSocketReleasesOutputEnvelopesAfterInterrupt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const keptItem = `{"type":"message","id":"msg_kept","role":"assistant","status":"completed","content":[{"type":"output_text","text":"kept"}]}`
		const cutItem = `{"type":"message","id":"msg_cut","role":"assistant","status":"completed","content":[{"type":"output_text","text":"discarded"}]}`
		type ownership struct {
			envelope, item weak.Pointer[byte]
		}
		probes := make(chan ownership, 2)
		sendItem := func(w *socketTurnWriter, item string) error {
			event := []byte(`{"type":"response.output_item.done","padding":"` + strings.Repeat("x", 256<<10) + `","output_index":0,"item":` + item + `}`)
			if err := w.event(event); err != nil {
				return err
			}
			if len(w.items) != 1 || string(w.items[0]) != item {
				return errors.New("output item was not decoded")
			}
			probes <- ownership{envelope: weak.Make(&event[0]), item: weak.Make(&w.items[0][0])}
			return nil
		}
		readProbe := func() ownership {
			t.Helper()
			select {
			case probe := <-probes:
				return probe
			default:
				t.Fatal("missing output allocation probe")
				return ownership{}
			}
		}
		check := func(name string, probe ownership, itemLive bool) {
			t.Helper()
			if probe.envelope.Value() != nil {
				t.Errorf("%s: the output event still retains its 256 KiB envelope", name)
			}
			if live := probe.item.Value() != nil; live != itemLive {
				t.Errorf("%s: output item live = %t, want %t", name, live, itemLive)
			}
		}
		var requests [][]string
		canceled := make(chan struct{})
		via := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read synthetic request: %v", err)
				return
			}
			var items []string
			for _, item := range gjson.GetBytes(body, "input").Array() {
				items = append(items, strings.Clone(item.Raw))
			}
			requests = append(requests, items)
			if gjson.GetBytes(body, "previous_response_id").Exists() {
				t.Error("the synthetic request still names a previous response")
			}
			id, item := "resp_fresh", ""
			switch len(requests) {
			case 1:
				id, item = "resp_kept", keptItem
			case 2:
				id, item = "resp_cut", cutItem
			}
			w := response.(*socketTurnWriter)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if err := w.event([]byte(`{"type":"response.created","response":{"id":"` + id + `"}}`)); err != nil {
				t.Errorf("send created: %v", err)
				return
			}
			if item != "" {
				if err := sendItem(w, item); err != nil {
					t.Errorf("send output item: %v", err)
					return
				}
			}
			if id == "resp_cut" {
				<-request.Context().Done()
				close(canceled)
				return
			}
			if err := w.event([]byte(`{"type":"response.completed","response":{"id":"` + id + `","status":"completed","output":[]}}`)); err != nil {
				t.Errorf("send completed: %v", err)
			}
		})

		peer, conn, upgrade := dialPipeResponsesSocket(t)
		peer.SetReadLimit(1 << 20)
		socket := responsesSocket{c: conn, ctx: t.Context(), path: upgrade.URL.Path, o: &socketOrigin{req: upgrade, via: via}}
		served := make(chan struct{})
		go func() {
			defer close(served)
			socket.serve(socketQueueBytes)
		}()
		defer func() {
			peer.CloseNow()
			<-served
		}()
		client := &sockClient{t: t, c: peer}
		if end := last(client.turn(create("", userHi))); end.Get("type").String() != "response.completed" || end.Get("response.id").String() != "resp_kept" {
			t.Fatalf("initial turn did not complete: %s", end.Get("type").String())
		}
		synctest.Wait()
		kept := readProbe()
		runtime.GC()
		check("completed turn", kept, true)

		client.send(create("resp_kept", userMore))
		events, err := client.until("response.output_item.done", "error")
		if err != nil || last(events).Get("item.id").String() != "msg_cut" {
			t.Fatalf("interrupted turn did not yield its item: %v", err)
		}
		synctest.Wait()
		cut := readProbe()
		runtime.GC()
		check("turn in progress", cut, true)

		client.send(`{"type":"response.interrupt","response_id":"resp_cut","mode":"discard_partial_items"}`)
		events, err = client.until("response.incomplete", "error")
		if err != nil || len(events) != 2 || events[0].Get("type").String() != "response.interrupt.accepted" ||
			events[0].Get("response_id").String() != "resp_cut" ||
			last(events).Get("response.incomplete_details.reason").String() != "interrupted" {
			t.Fatalf("did not accept interrupt and end the turn: %v, %v", events, err)
		}
		synctest.Wait()
		select {
		case <-canceled:
		default:
			t.Fatal("the canceled turn is still running")
		}
		runtime.GC()
		check("older completed turn after interrupt", kept, true)
		check("interrupted turn after joining", cut, false)

		end := last(client.turn(create("resp_cut", userMore)))
		if end.Get("status").Int() != http.StatusBadRequest || end.Get("error.code").String() != "previous_response_not_found" {
			t.Fatalf("interrupted id was accepted: %s", end.Raw)
		}
		synctest.Wait()
		if len(requests) != 2 || !slices.Equal(requests[1], []string{userHi, keptItem, userMore}) {
			t.Fatalf("expected two requests and the completed item in the continuation, got %q", requests)
		}

		end = last(client.turn(create("", userMore)))
		if end.Get("type").String() != "response.completed" || end.Get("response.id").String() != "resp_fresh" {
			t.Fatalf("a full turn did not reuse the socket: %s", end.Raw)
		}
		synctest.Wait()
		if len(requests) != 3 || !slices.Equal(requests[2], []string{userMore}) {
			t.Fatalf("fresh full turn did not dispatch exactly its own input: %q", requests)
		}
	})
}

func BenchmarkSocketOutputItem(b *testing.B) {
	ctx, cancel := context.WithCancel(b.Context())
	cancel()
	event := []byte(`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` +
		strings.Repeat("x", 128<<10) + `"}]}}`)
	writer := socketTurnWriter{ctx: ctx}
	b.ReportAllocs()
	for b.Loop() {
		_ = writer.event(event)
		writer.items = writer.items[:0]
	}
}
