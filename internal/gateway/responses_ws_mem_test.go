package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/yetone/magpie/internal/provider"
)

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
	_, server := sockServer(t)
	client, _, err := dialSock(t, server, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	item := strings.Replace(userPing, "ping", strings.Repeat("x", 256<<10), 1)
	if end := last(client.turn(create("", item))); end.Get("type").String() != "response.completed" {
		t.Fatalf("turn ended as %s", end.Raw)
	}
	if !<-reused {
		t.Fatal("gateway routed a copy of the socket's POST body")
	}
	if body := up.body(0); !strings.Contains(string(body), item) {
		t.Fatal("upstream did not receive the complete input item")
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
