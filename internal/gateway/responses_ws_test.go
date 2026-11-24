package gateway

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/budget"
	"github.com/yetone/magpie/internal/codexcat"
	"github.com/yetone/magpie/internal/provider"
)

// respUp is a Responses provider that answers each turn with one message
// and a usage of 300 in and 50 out, keeping the bodies it was sent.
type respUp struct {
	mu     sync.Mutex
	bodies [][]byte
	head   []http.Header
	reply  func(w http.ResponseWriter, r *http.Request, n int) // nil: the usual
}

func (u *respUp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.bodies = append(u.bodies, b)
	u.head = append(u.head, r.Header.Clone())
	n := len(u.bodies)
	reply := u.reply
	u.mu.Unlock()
	if reply != nil {
		reply(w, r, n)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	id := fmt.Sprintf("resp_up%d", n)
	item := fmt.Sprintf(`{"type":"message","id":"msg_%d","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer %d"}]}`, n, n)
	io.WriteString(w, sse(
		`data: {"type":"response.created","response":{"id":"`+id+`","object":"response","status":"in_progress","output":[]}}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
		`data: {"type":"response.completed","response":{"id":"`+id+`","object":"response","status":"completed","output":[`+item+`],"usage":{"input_tokens":300,"output_tokens":50,"total_tokens":350}}}`))
}

func (u *respUp) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bodies)
}

func (u *respUp) body(i int) []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.bodies[i]
}

// sockUp saves a Responses provider served by u.
func sockUp(t *testing.T, u *respUp) {
	t.Helper()
	up := httptest.NewServer(u)
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "fake", Name: "Fake", Key: "k", Responses: up.URL + "/v1", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
}

// sockServer serves the gateway as the real server does (Front), where a
// request's address and Host can be set by the test (X-Test-Remote,
// X-Test-Host): a test's dial comes from 127.0.0.1 to the server's own.
func sockServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := New()
	return s, serveSock(t, s)
}

func serveSock(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	front := s.Front()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("X-Test-Remote"); v != "" {
			r.RemoteAddr = v
		}
		if v := r.Header.Get("X-Test-Host"); v != "" {
			r.Host = v
		}
		r.Header.Del("X-Test-Remote")
		r.Header.Del("X-Test-Host")
		front.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	// a socket's handler outlives ts.Close (hijacked): it ends before
	// the test's folders go
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.sockets.drain(ctx)
	})
	return ts
}

type sockClient struct {
	t *testing.T
	c *websocket.Conn
}

func dialSock(t *testing.T, ts *httptest.Server, path string, h http.Header) (*sockClient, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+path, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, resp, err
	}
	c.SetReadLimit(64 << 20)
	t.Cleanup(func() { c.CloseNow() })
	return &sockClient{t: t, c: c}, resp, nil
}

func (s *sockClient) send(frame string) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.c.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		s.t.Fatal(err)
	}
}

// until reads frames up to and with one of the given types, or until the
// socket ends (err).
func (s *sockClient) until(kinds ...string) ([]gjson.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out []gjson.Result
	for {
		typ, b, err := s.c.Read(ctx)
		if err != nil {
			return out, err
		}
		if typ != websocket.MessageText {
			s.t.Fatalf("a %v frame", typ)
		}
		r := gjson.ParseBytes(b)
		out = append(out, r)
		for _, k := range kinds {
			if r.Get("type").String() == k {
				return out, nil
			}
		}
	}
}

func (s *sockClient) turn(frame string) []gjson.Result {
	s.t.Helper()
	s.send(frame)
	got, err := s.until("response.completed", "response.failed", "response.incomplete", "error")
	if err != nil {
		s.t.Fatalf("%v after %v", err, got)
	}
	return got
}

func last(rs []gjson.Result) gjson.Result { return rs[len(rs)-1] }

const userHi = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
const userMore = `{"type":"message","role":"user","content":[{"type":"input_text","text":"and more"}]}`

func create(prev, input string, extra ...string) string {
	f := `{"type":"response.create","model":"fake/m1","stream":true,"store":false,"instructions":"be brief","input":[` + input + `]`
	if prev != "" {
		f += `,"previous_response_id":"` + prev + `"`
	}
	for _, e := range extra {
		f += "," + e
	}
	return f + "}"
}

// A response.create streams whatever it says of stream (#5291):
// OpenAI's WebSocket mode doesn't use the HTTP field, so a client leaves
// it out, and the turn went to the provider unstreamed, its reply coming
// back whole at the end. The provider here holds its reply open after
// the first delta, which must reach the client before it finishes.
func TestResponsesSocketStreamsWithoutStreamField(t *testing.T) {
	fresh(t)
	for _, path := range []string{"/v1/responses", "/responses"} {
		for _, stream := range []string{"", `"stream":false,`} {
			name := path + " " + cmp.Or(stream, "no stream")
			finish := make(chan struct{}, 1)
			u := &respUp{}
			u.reply = func(w http.ResponseWriter, r *http.Request, n int) {
				item := `{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello"}]}`
				response := `{"id":"resp_up1","object":"response","status":"completed","output":[` + item + `],"usage":{"input_tokens":300,"output_tokens":50,"total_tokens":350}}`
				if !gjson.GetBytes(u.body(n-1), "stream").Bool() {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, response)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, sse(
					`data: {"type":"response.created","response":{"id":"resp_up1","object":"response","status":"in_progress","output":[]}}`,
					`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[]}}`,
					`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}`))
				w.(http.Flusher).Flush()
				select {
				case <-finish:
				case <-r.Context().Done():
					return
				}
				io.WriteString(w, sse(
					`data: {"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
					`data: {"type":"response.completed","response":`+response+`}`))
			}
			sockUp(t, u)
			_, ts := sockServer(t)
			c, _, err := dialSock(t, ts, path, nil)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			c.send(`{"type":"response.create","model":"fake/m1",` + stream + `"store":false,"input":[` + userHi + `]}`)
			got, err := c.until("response.output_text.delta", "response.completed", "response.failed", "error")
			if err != nil {
				close(finish)
				t.Fatalf("%s: waiting for a text delta: %v after %v", name, err, got)
			}
			if delta := last(got); delta.Get("type").String() != "response.output_text.delta" || delta.Get("delta").String() != "hello" {
				close(finish)
				t.Fatalf("%s: no text delta before the upstream completed: %v", name, got)
			}
			finish <- struct{}{}
			got, err = c.until("response.completed", "response.failed", "error")
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			end := last(got)
			if end.Get("type").String() != "response.completed" || end.Get("response.output.0.content.0.text").String() != "hello" || end.Get("response.usage.total_tokens").Int() != 350 {
				t.Fatalf("%s: completed reply: %v", name, got)
			}
			if !gjson.GetBytes(u.body(0), "stream").Bool() {
				t.Errorf("%s: upstream body %s", name, u.body(0))
			}
		}
	}
}

// The same on Codex's own route (#5291): a frame without stream reaches
// the ChatGPT backend as a streamed turn.
func TestCodexSocketStreamsWithoutStreamField(t *testing.T) {
	setup(t, provider.Chat, &fake{t: t})
	bodies := make(chan []byte, 1)
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case bodies <- b:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}`
		io.WriteString(w, sse(
			`data: {"type":"response.created","response":{"id":"resp_c1"}}`,
			`data: {"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
			`data: {"type":"response.completed","response":{"id":"resp_c1","usage":{"input_tokens":9,"output_tokens":2}}}`))
	})
	_, ts := sockServer(t)
	h := http.Header{"Authorization": {"Bearer chatgpt-token"}, "Chatgpt-Account-Id": {"acct-1"},
		"Openai-Beta": {"responses_websockets=2026-02-06"}, "User-Agent": {"codex_cli_rs/0.166.0"}}
	c, _, err := dialSock(t, ts, CodexPath+"/responses", h)
	if err != nil {
		t.Fatal(err)
	}
	got := c.turn(`{"type":"response.create","model":"gpt-5.5","store":false,"input":[` + userHi + `]}`)
	if last(got).Get("type").String() != "response.completed" {
		t.Fatalf("%v", got)
	}
	select {
	case b := <-bodies:
		if !gjson.GetBytes(b, "stream").Bool() {
			t.Errorf("sent to ChatGPT unstreamed: %s", b)
		}
	default:
		t.Fatal("nothing reached ChatGPT")
	}
}

// Codex's socket is served (#4058): the upgrade is taken on each
// Responses route; a turn naming the reply before it, with only what
// came after, reaches the provider as the whole conversation, the reply
// in its place, and none of the socket's own members. Over HTTP Codex
// sends the conversation whole each turn; magpie answered the upgrade
// 426 (a87dc1ff2), so it never sent less.
func TestResponsesSocketContinues(t *testing.T) {
	fresh(t)
	for _, path := range []string{"/v1/responses", "/responses"} {
		u := &respUp{}
		sockUp(t, u)
		_, ts := sockServer(t)
		c, resp, err := dialSock(t, ts, path, nil)
		if err != nil {
			code := 0
			if resp != nil {
				code = resp.StatusCode
			}
			t.Fatalf("%s: %v (%d)", path, err, code)
		}
		got := c.turn(create("", userHi))
		end := last(got)
		if end.Get("type").String() != "response.completed" {
			t.Fatalf("%s turn 1: %v", path, got)
		}
		id := end.Get("response.id").String()
		if id == "" || got[0].Get("type").String() != "response.created" {
			t.Fatalf("%s turn 1: %v", path, got)
		}
		var outputItem string
		for _, f := range got {
			if f.Get("type").String() == "response.output_item.done" {
				outputItem = f.Get("item").Raw
			}
		}
		if outputItem == "" {
			t.Fatalf("%s: no item: %v", path, got)
		}
		got = c.turn(create(id, userMore))
		if last(got).Get("type").String() != "response.completed" {
			t.Fatalf("%s turn 2: %v", path, got)
		}
		if u.calls() != 2 {
			t.Fatalf("%s: %d calls", path, u.calls())
		}
		b := u.body(1)
		in := gjson.GetBytes(b, "input").Array()
		if len(in) != 3 || in[0].Get("content.0.text").String() != "hi" || in[1].Get("role").String() != "assistant" ||
			in[1].Get("content.0.text").String() != "answer 1" || in[2].Get("content.0.text").String() != "and more" {
			t.Errorf("%s turn 2 upstream input: %s", path, b)
		} else if in[1].Raw != outputItem {
			t.Errorf("%s: continued item %s differs from streamed item %s", path, in[1].Raw, outputItem)
		}
		for _, k := range []string{"type", "previous_response_id", "generate"} {
			if gjson.GetBytes(b, k).Exists() {
				t.Errorf("%s: %s went upstream: %s", path, k, b)
			}
		}
		if gjson.GetBytes(b, "instructions").String() != "be brief" {
			t.Errorf("%s: %s", path, b)
		}
		// a third turn continues the second
		got = c.turn(create(last(got).Get("response.id").String(), userHi))
		if last(got).Get("type").String() != "response.completed" || len(gjson.GetBytes(u.body(2), "input").Array()) != 5 {
			t.Errorf("%s turn 3: %v %s", path, got, u.body(2))
		}
	}
}

// A plain GET on a Responses route is still told to POST.
func TestResponsesSocketPlainGET(t *testing.T) {
	fresh(t)
	sockUp(t, &respUp{})
	for _, path := range []string{"/v1/responses", "/responses"} {
		rec := httptest.NewRecorder()
		New().Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
			t.Errorf("%s GET: %d %q", path, rec.Code, rec.Header().Get("Allow"))
		}
	}
}

// A reply the socket doesn't hold is previous_response_not_found, on
// which Codex opens a new socket with the conversation whole; nothing goes
// upstream. So is one from another socket: each holds its own.
func TestResponsesSocketUnknownPrevious(t *testing.T) {
	fresh(t)
	u := &respUp{}
	sockUp(t, u)
	_, ts := sockServer(t)
	a, _, err := dialSock(t, ts, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := a.turn(create("", userHi))
	id := last(got).Get("response.id").String()
	b, _, err := dialSock(t, ts, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, prev := range []string{"resp_never", id} {
		got = b.turn(create(prev, userMore))
		e := last(got)
		if e.Get("type").String() != "error" || e.Get("status").Int() != 400 || e.Get("error.code").String() != "previous_response_not_found" {
			t.Errorf("%s: %v", prev, got)
		}
	}
	if u.calls() != 1 {
		t.Errorf("%d calls", u.calls())
	}
	// the socket goes on
	if got = b.turn(create("", userHi)); last(got).Get("type").String() != "response.completed" {
		t.Errorf("after: %v", got)
	}
}

// A warmup (generate false) is answered without a provider; the turn
// after it names it and sends only what came after, and the provider gets
// the whole.
func TestResponsesSocketWarmup(t *testing.T) {
	fresh(t)
	u := &respUp{}
	sockUp(t, u)
	_, ts := sockServer(t)
	c, _, err := dialSock(t, ts, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := c.turn(create("", userHi, `"generate":false`))
	if len(got) != 2 || got[0].Get("type").String() != "response.created" || last(got).Get("type").String() != "response.completed" {
		t.Fatalf("warmup: %v", got)
	}
	if u.calls() != 0 {
		t.Fatalf("a warmup went upstream")
	}
	got = c.turn(create(last(got).Get("response.id").String(), userMore))
	if last(got).Get("type").String() != "response.completed" || u.calls() != 1 {
		t.Fatalf("turn: %v", got)
	}
	in := gjson.GetBytes(u.body(0), "input").Array()
	if len(in) != 2 || in[0].Get("content.0.text").String() != "hi" || in[1].Get("content.0.text").String() != "and more" {
		t.Errorf("upstream: %s", u.body(0))
	}
}

// A refused turn is an error frame with the HTTP status and error object,
// as Codex reads one; the socket goes on, and the refused turn isn't one a
// later turn can name.
func TestResponsesSocketUpstreamError(t *testing.T) {
	fresh(t)
	u := &respUp{}
	u.reply = func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-bad")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"too long for this model"}}`)
	}
	sockUp(t, u)
	_, ts := sockServer(t)
	c, _, err := dialSock(t, ts, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := c.turn(create("", userHi))
	e := last(got)
	if e.Get("type").String() != "error" || e.Get("status").Int() != 400 || !strings.Contains(e.Get("error.message").String(), "too long") {
		t.Fatalf("%v", got)
	}
	u.mu.Lock()
	u.reply = nil
	u.mu.Unlock()
	if got = c.turn(create("", userHi)); last(got).Get("type").String() != "response.completed" {
		t.Errorf("after: %v", got)
	}
}

// An interrupt of the reply in flight stops it: response.interrupt
// .accepted and an interrupted response.incomplete, as OpenAI's socket
// answers Codex; the provider's request is cancelled, and the socket
// serves the next turn. One naming another reply fails.
func TestResponsesSocketInterrupt(t *testing.T) {
	fresh(t)
	u := &respUp{}
	gone := make(chan struct{}, 1)
	u.reply = func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`data: {"type":"response.created","response":{"id":"resp_long","object":"response","status":"in_progress","output":[]}}`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_long","role":"assistant","status":"in_progress","content":[]}}`,
			`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_long","delta":"wor"}`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		gone <- struct{}{}
	}
	sockUp(t, u)
	_, ts := sockServer(t)
	c, _, err := dialSock(t, ts, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.send(create("", userHi))
	if got, err := c.until("response.created"); err != nil || last(got).Get("response.id").String() != "resp_long" {
		t.Fatalf("%v %v", got, err)
	}
	c.send(`{"type":"response.interrupt","response_id":"resp_other","mode":"discard_partial_items"}`)
	got, err := c.until("response.interrupt.failed")
	if err != nil || last(got).Get("response_id").String() != "resp_other" {
		t.Fatalf("%v %v", got, err)
	}
	c.send(`{"type":"response.interrupt","response_id":"resp_long","mode":"discard_partial_items"}`)
	got, err = c.until("response.incomplete")
	if err != nil {
		t.Fatalf("%v %v", got, err)
	}
	var accepted bool
	for _, f := range got {
		accepted = accepted || (f.Get("type").String() == "response.interrupt.accepted" && f.Get("response_id").String() == "resp_long")
	}
	end := last(got)
	if !accepted || end.Get("response.id").String() != "resp_long" || end.Get("response.incomplete_details.reason").String() != "interrupted" {
		t.Fatalf("%v", got)
	}
	select {
	case <-gone:
	case <-time.After(30 * time.Second):
		t.Fatal("the provider's request went on")
	}
	u.mu.Lock()
	u.reply = nil
	u.mu.Unlock()
	if got = c.turn(create("", userHi)); last(got).Get("type").String() != "response.completed" {
		t.Errorf("after: %v", got)
	}
}

func holdResponsesSocket(t *testing.T, path string, frameLimit int64) (*sockClient, *httptest.Server, *respUp, <-chan struct{}) {
	t.Helper()
	fresh(t)
	stopped := make(chan struct{})
	up := &respUp{}
	up.reply = func(w http.ResponseWriter, r *http.Request, _ int) {
		up.mu.Lock()
		up.reply = nil
		up.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(
			`data: {"type":"response.created","response":{"id":"resp_wait","object":"response","status":"in_progress","output":[]}}`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_wait","role":"assistant","status":"in_progress","content":[]}}`,
			`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_wait","delta":"partial"}`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(stopped)
	}
	sockUp(t, up)
	s := New()
	s.requestLimits.body = frameLimit
	ts := serveSock(t, s)
	c, _, err := dialSock(t, ts, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.send(create("", userHi))
	events, err := c.until("response.output_text.delta", "error")
	if err != nil || len(events) == 0 || events[0].Get("response.id").String() != "resp_wait" ||
		last(events).Get("delta").String() != "partial" {
		t.Fatalf("first response: %v, %v", events, err)
	}
	return c, ts, up, stopped
}

func TestResponsesSocketQueuedTurnsRunInOrderAfterInterrupt(t *testing.T) {
	c, _, up, stopped := holdResponsesSocket(t, "/v1/responses", 0)
	const pending = 8
	for number := range pending {
		input := strings.Replace(userHi, `"hi"`, fmt.Sprintf(`"queued %d"`, number), 1)
		c.send(create("", input))
	}
	c.send(`{"type":"response.interrupt","response_id":"other","mode":"discard_partial_items"}`)
	if events, err := c.until("response.interrupt.failed", "response.completed", "error"); err != nil ||
		last(events).Get("type").String() != "response.interrupt.failed" || up.calls() != 1 {
		t.Fatalf("before interrupt: %v, %v, %d upstream calls", events, err, up.calls())
	}
	c.send(`{"type":"response.interrupt","response_id":"resp_wait","mode":"discard_partial_items"}`)
	events, err := c.until("response.incomplete", "error")
	if err != nil || len(events) != 2 || events[0].Get("type").String() != "response.interrupt.accepted" ||
		last(events).Get("response.incomplete_details.reason").String() != "interrupted" {
		t.Fatalf("interrupt: %v, %v", events, err)
	}
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("interrupted upstream request did not stop")
	}
	for number := range pending {
		events, err := c.until("response.completed", "error")
		if err != nil || last(events).Get("type").String() != "response.completed" {
			t.Fatalf("queued turn %d: %v, %v", number, events, err)
		}
		want := fmt.Sprintf("queued %d", number)
		if got := gjson.GetBytes(up.body(number+1), "input.0.content.0.text").String(); got != want {
			t.Errorf("queued turn %d: upstream got %q, want %q", number, got, want)
		}
	}
	if calls := up.calls(); calls != pending+1 {
		t.Errorf("upstream calls = %d, want %d", calls, pending+1)
	}
}

func TestResponsesSocketPendingLimitStopsBeforeDispatch(t *testing.T) {
	for _, test := range []struct {
		name, path string
		frameLimit int64
		frame      string
		count      int
	}{
		{name: "count", path: "/v1/responses", frame: create("", userMore), count: 9},
		{
			name: "bytes", path: CodexPath + "/responses", frameLimit: 8 << 10,
			frame: create("", strings.Replace(userHi, "hi", strings.Repeat("x", 5<<10), 1)),
			count: 2,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, ts, up, stopped := holdResponsesSocket(t, test.path, test.frameLimit)
			for range test.count {
				c.send(test.frame)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			_ = c.c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.interrupt","response_id":"other"}`))
			events, err := c.until("response.interrupt.failed", "response.completed", "error")
			if websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
				t.Fatalf("pending limit: close = %v, frames = %v", err, events)
			}
			select {
			case <-stopped:
			case <-ctx.Done():
				t.Fatal("overflow did not stop the upstream request")
			}
			if calls := up.calls(); calls != 1 {
				t.Fatalf("upstream calls = %d, want only the first turn", calls)
			}
			reconnected, _, err := dialSock(t, ts, test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			end := last(reconnected.turn(create("resp_wait", userMore)))
			if end.Get("type").String() != "error" || end.Get("status").Int() != http.StatusBadRequest ||
				end.Get("error.code").String() != "previous_response_not_found" || up.calls() != 1 {
				t.Fatalf("old continuation after reconnect: %s, %d upstream calls", end.Raw, up.calls())
			}
			end = last(reconnected.turn(create("", userMore+","+userHi)))
			if end.Get("type").String() != "response.completed" || up.calls() != 2 {
				t.Fatalf("full request after reconnect: %s, %d upstream calls", end.Raw, up.calls())
			}
			request := gjson.ParseBytes(up.body(1))
			input := request.Get("input").Array()
			if request.Get("previous_response_id").Exists() || len(input) != 2 ||
				input[0].Get("content.0.text").String() != "and more" || input[1].Get("content.0.text").String() != "hi" {
				t.Fatalf("full request after reconnect reached upstream as %s", request.Raw)
			}
		})
	}
}

func TestResponsesSocketPendingPayloadFits(t *testing.T) {
	const budget = 8 << 10
	first := create("", userHi)
	second := create("", userMore)
	firstSize := 3<<10 + 1
	c, _, up, stopped := holdResponsesSocket(t, CodexPath+"/responses", budget)
	c.send(first + strings.Repeat(" ", firstSize-len(first)))
	c.send(second + strings.Repeat(" ", budget-firstSize-len(second)))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_ = c.c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.interrupt","response_id":"resp_wait"}`))
	events, err := c.until("response.incomplete", "error")
	if err != nil || last(events).Get("response.incomplete_details.reason").String() != "interrupted" {
		t.Fatalf("pending payloads used exactly %d bytes: interrupt = %v, %v", budget, events, err)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("interrupted upstream request did not stop")
	}
	for index, want := range []string{"hi", "and more"} {
		events, err := c.until("response.completed", "error")
		if err != nil || last(events).Get("type").String() != "response.completed" {
			t.Fatalf("pending turn %d: %v, %v", index, events, err)
		}
		if got := gjson.GetBytes(up.body(index+1), "input.0.content.0.text").String(); got != want {
			t.Errorf("pending turn %d upstream input = %q, want %q", index, got, want)
		}
	}
	if calls := up.calls(); calls != 3 {
		t.Errorf("upstream calls = %d, want 3", calls)
	}
}

type pipeSocketUpgrade struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (w pipeSocketUpgrade) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func dialPipeResponsesSocket(t *testing.T) (*websocket.Conn, *websocket.Conn, *http.Request) {
	t.Helper()
	server, peer := net.Pipe()
	t.Cleanup(func() { server.Close() })
	t.Cleanup(func() { peer.Close() })
	var upgrade *http.Request
	var conn *websocket.Conn
	client, _, err := websocket.Dial(t.Context(), "ws://example.test/v1/responses", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			upgrade = req
			response := httptest.NewRecorder()
			var err error
			conn, err = websocket.Accept(pipeSocketUpgrade{response, server}, req, nil)
			if err != nil {
				return nil, err
			}
			accepted := response.Result()
			accepted.Body = peer
			return accepted, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.CloseNow() })
	return client, conn, upgrade
}

func TestResponsesSocketOverflowDuringWrite(t *testing.T) {
	for _, test := range []struct {
		name  string
		drain bool
	}{
		{name: "peer stays blocked"},
		{name: "peer resumes in time for 1013", drain: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				client, conn, upgrade := dialPipeResponsesSocket(t)
				started := make(chan struct{})
				canceled := make(chan struct{})
				requests := 0
				via := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					if requests != 1 {
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, sse(`data: {"type":"response.created","response":{"id":"resp_wait","output":[]}}`))
					close(started)
					<-r.Context().Done()
					close(canceled)
				})
				socket := responsesSocket{c: conn, ctx: ctx, path: upgrade.URL.Path, o: &socketOrigin{req: upgrade, via: via}}
				served := make(chan struct{})
				go func() {
					socket.serve(socketQueueBytes)
					close(served)
				}()
				if err := client.Write(ctx, websocket.MessageText, []byte(create("", userHi))); err != nil {
					t.Fatal(err)
				}
				<-started
				synctest.Wait()
				overflowAt := time.Now()
				for range socketQueueFrames + 1 {
					if err := client.Write(ctx, websocket.MessageText, []byte(create("", userMore))); err != nil {
						t.Fatal(err)
					}
				}
				<-canceled
				if delay := time.Since(overflowAt); delay != 0 {
					t.Errorf("provider cancellation took %v", delay)
				}
				if test.drain {
					time.Sleep(250 * time.Millisecond)
					_, first, err := client.Read(ctx)
					if err != nil || gjson.GetBytes(first, "type").String() != "response.created" {
						t.Fatalf("in-flight frame = %q, %v", first, err)
					}
					if _, _, err := client.Read(ctx); websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
						t.Fatalf("close after writes resume = %v, want 1013", err)
					}
				}
				<-served
				if delay := time.Since(overflowAt); delay > 2*time.Second {
					t.Errorf("overflow held the blocked socket for %v", delay)
				}
				if requests != 1 {
					t.Errorf("overflow dispatched %d requests, want 1", requests)
				}
			})
		})
	}
}

func TestResponsesSocketInterruptDuringWrite(t *testing.T) {
	for _, test := range []struct {
		name   string
		resume bool
	}{
		{name: "peer resumes and reuses the socket", resume: true},
		{name: "peer never reads again"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peer, conn, upgrade := dialPipeResponsesSocket(t)
				defer peer.CloseNow()
				deltaRead, canceled, served := make(chan struct{}), make(chan struct{}), make(chan struct{})
				requests := 0
				via := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					w.Header().Set("Content-Type", "text/event-stream")
					if requests != 1 {
						io.WriteString(w, sse(`data: {"type":"response.completed","response":{"id":"resp_fresh","object":"response","status":"completed","output":[]}}`))
						return
					}
					io.WriteString(w, sse(`data: {"type":"response.created","response":{"id":"resp_wait","object":"response","status":"in_progress","output":[]}}`))
					io.WriteString(w, sse(`data: {"type":"response.output_text.delta","item_id":"msg_wait","output_index":0,"content_index":0,"delta":"partial"}`))
					close(deltaRead)
					<-r.Context().Done()
					close(canceled)
				})
				socket := responsesSocket{c: conn, ctx: t.Context(), path: upgrade.URL.Path, o: &socketOrigin{req: upgrade, via: via}}
				go func() {
					socket.serve(socketQueueBytes)
					close(served)
				}()
				client := &sockClient{t: t, c: peer}
				client.send(create("", userHi))
				events, err := client.until("response.created")
				if err != nil || last(events).Get("response.id").String() != "resp_wait" {
					t.Fatalf("first reply was not created: %v", err)
				}
				<-deltaRead
				synctest.Wait()
				prev := ""
				if test.resume {
					prev = "resp_wait"
				}
				client.send(create(prev, userMore))
				interruptedAt := time.Now()
				client.send(`{"type":"response.interrupt","response_id":"resp_wait"}`)
				synctest.Wait()
				select {
				case <-canceled:
				default:
					t.Fatal("interrupt did not cancel the upstream while the socket write was blocked")
				}
				if delay := time.Since(interruptedAt); delay != 0 {
					t.Fatalf("upstream cancellation waited %s for the client", delay)
				}
				if !test.resume {
					time.Sleep(socketWriteTimeout)
					synctest.Wait()
					select {
					case <-served:
					default:
						t.Fatal("socket stayed open past the write deadline")
					}
					if requests != 1 {
						t.Fatalf("stalled socket dispatched %d requests, want only the first", requests)
					}
					return
				}
				time.Sleep(250 * time.Millisecond)
				events, err = client.until("response.incomplete", "error")
				if err != nil || len(events) != 3 ||
					events[0].Get("type").String() != "response.output_text.delta" ||
					events[1].Get("type").String() != "response.interrupt.accepted" ||
					last(events).Get("response.incomplete_details.reason").String() != "interrupted" {
					t.Fatalf("wrong interrupt sequence after reads resumed: %v, %v", events, err)
				}
				events, err = client.until("error", "response.completed")
				if err != nil || last(events).Get("error.code").String() != "previous_response_not_found" {
					t.Fatalf("queued continuation of interrupted response: %v, %v", events, err)
				}
				if end := last(client.turn(create("", userMore))); end.Get("response.id").String() != "resp_fresh" {
					t.Fatalf("a fresh turn could not reuse the socket: %v", end)
				}
				synctest.Wait()
				if requests != 2 {
					t.Fatalf("resumed socket dispatched %d requests, want the canceled and fresh turns", requests)
				}
			})
		})
	}
}

func TestResponsesSocketWriteDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server, peer := net.Pipe()
		defer server.Close()
		defer peer.Close()
		req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		conn, err := websocket.Accept(pipeSocketUpgrade{httptest.NewRecorder(), server}, req, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseNow()
		live, closeSocket := context.WithCancel(t.Context())
		defer closeSocket()
		socket := responsesSocket{c: conn, live: live, cancel: closeSocket}
		drained := make(chan error, 1)
		go func() {
			buf := make([]byte, 2+len("first"))
			_, err := io.ReadFull(peer, buf)
			drained <- err
		}()
		if err := socket.write(live, []byte("first")); err != nil {
			t.Fatal(err)
		}
		if err := <-drained; err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Minute)

		turn, interrupt := context.WithCancel(live)
		defer interrupt()
		wrote := make(chan error, 1)
		go func() { wrote <- socket.write(turn, []byte("unread")) }()
		synctest.Wait()
		interrupt()
		synctest.Wait()
		select {
		case err := <-wrote:
			t.Fatalf("an interrupt closed a write in progress: %v", err)
		default:
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		select {
		case err := <-wrote:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("blocked write = %v, want deadline exceeded", err)
			}
		default:
			t.Fatal("a client that does not read still holds the socket after a minute")
		}
		if live.Err() != context.Canceled {
			t.Errorf("socket context = %v after write timeout", live.Err())
		}
	})
}

// A reply cut before its end is a failed turn, as the HTTP route tells
// it; the socket goes on, and the failed reply isn't one to name.
func TestResponsesSocketCutReplyFails(t *testing.T) {
	fresh(t)
	u := &respUp{}
	u.reply = func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`data: {"type":"response.created","response":{"id":"resp_cut","object":"response","status":"in_progress","output":[]}}`,
			`data: {"type":"response.output_text.delta","delta":"par"}`))
	}
	sockUp(t, u)
	_, ts := sockServer(t)
	c, _, err := dialSock(t, ts, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := c.turn(create("", userHi))
	if last(got).Get("type").String() != "response.failed" {
		t.Fatalf("%v", got)
	}
	got = c.turn(create("resp_cut", userMore))
	if last(got).Get("error.code").String() != "previous_response_not_found" {
		t.Errorf("%v", got)
	}
}

// Everything that judges an HTTP turn judges each turn on the socket: a
// remote caller needs a key to open it, a key turned off refuses the next
// turn, a key's limit counts each turn; a web page elsewhere, a sandboxed
// frame (Origin null), a localhost subdomain and a rebound name can't open
// one at all.
func TestResponsesSocketGuards(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "")
	t.Setenv("MAGPIE_PUBLIC_URL", "")
	budget.Forget()
	pinLimitClock(t)
	u := &respUp{}
	sockUp(t, u)
	keys, secrets := newCaller(t, "Laptop", "Capped")
	_, ts := sockServer(t)
	remote := func(secret string) http.Header {
		h := http.Header{"X-Test-Remote": {"192.168.1.9:5000"}}
		if secret != "" {
			h.Set("Authorization", "Bearer "+secret)
		}
		return h
	}
	refused := func(what string, h http.Header) {
		t.Helper()
		c, resp, err := dialSock(t, ts, "/v1/responses", h)
		if err == nil {
			// opened: its turn must be refused, never served
			got := c.turn(create("", userHi))
			t.Errorf("%s opened a socket: %v", what, got)
			return
		}
		if resp == nil || resp.StatusCode < 400 {
			t.Errorf("%s: %v %v", what, err, resp)
		}
	}
	before := u.calls()
	refused("a remote caller without a key", remote(""))
	refused("a remote caller with a wrong key", remote("sk-magpie-wrong"))
	for _, origin := range []string{"https://evil.example", "null", "http://evil.localhost:3425"} {
		refused("Origin "+origin, http.Header{"Origin": {origin}})
	}
	refused("a rebound page", http.Header{
		"X-Test-Host": {"rebind.attacker.example:3425"},
		"User-Agent":  {"Mozilla/5.0 (Macintosh) Chrome/140"}, "Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"websocket"},
	})
	if u.calls() != before {
		t.Fatalf("a refused socket reached the provider")
	}

	// a key: served, until it is turned off
	c, _, err := dialSock(t, ts, "/v1/responses", remote(secrets[0]))
	if err != nil {
		t.Fatal(err)
	}
	got := c.turn(create("", userHi))
	if last(got).Get("type").String() != "response.completed" {
		t.Fatalf("keyed: %v", got)
	}
	if h := u.head[len(u.head)-1].Get("Authorization"); h != "Bearer k" {
		t.Errorf("the provider got %q", h)
	}
	if _, err := access.Update("off-key", access.Change{Key: keys[0].ID}); err != nil {
		t.Fatal(err)
	}
	got = c.turn(create(last(got).Get("response.id").String(), userMore))
	if e := last(got); e.Get("type").String() != "error" || e.Get("status").Int() != http.StatusUnauthorized {
		t.Errorf("a key turned off: %v", got)
	}
	if u.calls() != 1 {
		t.Errorf("%d calls", u.calls())
	}

	// a limit counts each turn: 350 tokens a turn, 700 allowed
	setLimit(t, keys[1].ID, &access.Limit{Period: "day", Tokens: 700})
	c, _, err = dialSock(t, ts, "/v1/responses", remote(secrets[1]))
	if err != nil {
		t.Fatal(err)
	}
	prev := ""
	for i := 0; i < 2; i++ {
		got = c.turn(create(prev, userHi))
		if last(got).Get("type").String() != "response.completed" {
			t.Fatalf("turn %d: %v", i, got)
		}
		prev = last(got).Get("response.id").String()
	}
	got = c.turn(create(prev, userMore))
	if e := last(got); e.Get("type").String() != "error" || e.Get("status").Int() != http.StatusTooManyRequests {
		t.Errorf("past the limit: %v", got)
	}
	if u.calls() != 3 {
		t.Errorf("%d calls", u.calls())
	}
}

// When the gateway ends, a socket is closed going away after its turn,
// and Codex opens a new one to the gateway serving the port then.
func TestResponsesSocketDrains(t *testing.T) {
	fresh(t)
	sockUp(t, &respUp{})
	s, ts := sockServer(t)
	c, _, err := dialSock(t, ts, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.turn(create("", userHi)); last(got).Get("type").String() != "response.completed" {
		t.Fatal(got)
	}
	// Codex reads all along, the close's answer too
	read := make(chan error, 1)
	go func() {
		_, err := c.until("never")
		read <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.sockets.drain(ctx)
	if ctx.Err() != nil {
		t.Fatal("drain waited out")
	}
	if err := <-read; websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Errorf("%v", err)
	}
	if _, resp, err := dialSock(t, ts, "/v1/responses", nil); err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a socket while the gateway ends: %v %v", err, resp)
	}
}

// Codex continues a turn on its socket by previous_response_id and sends
// no turn state, the socket being the turn's routing; magpie sends each
// request on whole, so the state the turn's last reply said goes with it,
// as Codex sends it over HTTP. A new turn starts without it.
func TestCodexSocketKeepsTurnState(t *testing.T) {
	setup(t, provider.Chat, &fake{t: t})
	var mu sync.Mutex
	var heads []http.Header
	n := 0
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		mu.Lock()
		heads = append(heads, r.Header.Clone())
		n++
		id := fmt.Sprintf("resp_k%d", n)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Turn-State", "ts-"+id)
		item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}`
		io.WriteString(w, sse(
			`data: {"type":"response.created","response":{"id":"`+id+`"}}`,
			`data: {"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
			`data: {"type":"response.completed","response":{"id":"`+id+`","usage":{"input_tokens":9,"output_tokens":2}}}`))
	})
	_, ts := sockServer(t)
	h := http.Header{"Authorization": {"Bearer chatgpt-token"}, "Chatgpt-Account-Id": {"acct-1"},
		"Openai-Beta": {"responses_websockets=2026-02-06"}, "User-Agent": {"codex_cli_rs/0.165.0"}}
	c, _, err := dialSock(t, ts, CodexPath+"/responses", h)
	if err != nil {
		t.Fatal(err)
	}
	frame := func(prev, turn, input string) string {
		f := `{"type":"response.create","model":"gpt-5.5","stream":true,"store":false,"input":[` + input + `],` +
			`"client_metadata":{"turn_id":"` + turn + `"}`
		if prev != "" {
			f += `,"previous_response_id":"` + prev + `"`
		}
		return f + "}"
	}
	prev := ""
	for _, turn := range []string{"t1", "t1", "t2"} {
		got := c.turn(frame(prev, turn, userHi))
		if last(got).Get("type").String() != "response.completed" {
			t.Fatalf("%v", got)
		}
		prev = last(got).Get("response.id").String()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heads) != 3 {
		t.Fatalf("%d calls", len(heads))
	}
	for i, want := range []string{"", "ts-resp_k1", ""} {
		if got := heads[i].Get("X-Codex-Turn-State"); got != want {
			t.Errorf("request %d: turn state %q, want %q", i+1, got, want)
		}
	}
}

// Codex's own route serves its socket too: each turn goes to the ChatGPT
// backend as Codex's HTTP turn would, the sign-in, the turn state and the
// whole conversation with it, and the reply's headers come back as the
// frames Codex reads them from on a socket (response.metadata,
// codex.response.metadata with magpie's model-list tag, codex.rate_limits).
func TestCodexSocketServed(t *testing.T) {
	setup(t, provider.Chat, &fake{t: t})
	var mu sync.Mutex
	var bodies [][]byte
	var heads []http.Header
	n := 0
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		heads = append(heads, r.Header.Clone())
		n++
		id := fmt.Sprintf("resp_c%d", n)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Models-Etag", `W/"v1"`)
		w.Header().Set("X-Codex-Turn-State", "ts-"+id)
		w.Header().Set("X-Codex-Primary-Used-Percent", "12.5")
		w.Header().Set("X-Codex-Primary-Window-Minutes", "300")
		item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}`
		io.WriteString(w, sse(
			`data: {"type":"response.created","response":{"id":"`+id+`"}}`,
			`data: {"type":"codex.response.metadata","headers":{"x-models-etag":"W/\"v1\"","x-other":"kept"}}`,
			`data: {"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
			`data: {"type":"response.completed","response":{"id":"`+id+`","usage":{"input_tokens":9,"output_tokens":2}}}`))
	})
	_, ts := sockServer(t)
	h := http.Header{"Authorization": {"Bearer chatgpt-token"}, "Chatgpt-Account-Id": {"acct-1"},
		"Openai-Beta": {"responses_websockets=2026-02-06"}, "User-Agent": {"codex_cli_rs/0.166.0"}}
	c, resp, err := dialSock(t, ts, CodexPath+"/responses", h)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("%v (%d)", err, code)
	}
	frame := func(prev, input string) string {
		f := `{"type":"response.create","model":"gpt-5.5","stream":true,"store":false,"input":[` + input + `],` +
			`"client_metadata":{"x-codex-turn-state":"ts-prev","ws_request_header_x_openai_internal_codex_responses_lite":"true"}`
		if prev != "" {
			f += `,"previous_response_id":"` + prev + `"`
		}
		return f + "}"
	}
	got := c.turn(frame("", userHi))
	if last(got).Get("type").String() != "response.completed" {
		t.Fatalf("%v", got)
	}
	tag := codexcat.Tag(provider.CodexListed())
	var meta, etag, limits bool
	for _, f := range got {
		switch f.Get("type").String() {
		case "response.metadata":
			meta = f.Get(`headers.x-codex-turn-state`).String() == "ts-resp_c1"
		case "codex.response.metadata":
			v := f.Get(`headers.x-models-etag`).String()
			if !codexcat.Tagged(v, tag) {
				t.Errorf("x-models-etag %q", v)
			}
			etag = etag || f.Get(`headers.x-other`).String() == "kept"
		case "codex.rate_limits":
			limits = f.Get("rate_limits.primary.used_percent").Float() == 12.5 && f.Get("rate_limits.primary.window_minutes").Int() == 300
		}
	}
	if !meta || !etag || !limits {
		t.Errorf("meta %v etag %v limits %v: %v", meta, etag, limits, got)
	}
	got = c.turn(frame(last(got).Get("response.id").String(), userMore))
	if last(got).Get("type").String() != "response.completed" {
		t.Fatalf("%v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("%d calls", len(bodies))
	}
	in := gjson.GetBytes(bodies[1], "input").Array()
	if len(in) != 3 || in[1].Get("role").String() != "assistant" || gjson.GetBytes(bodies[1], "previous_response_id").Exists() || gjson.GetBytes(bodies[1], "type").Exists() {
		t.Errorf("turn 2: %s", bodies[1])
	}
	hd := heads[1]
	if hd.Get("Authorization") != "Bearer chatgpt-token" || hd.Get("Chatgpt-Account-Id") != "acct-1" ||
		hd.Get("X-Codex-Turn-State") != "ts-prev" || strings.Contains(hd.Get("Openai-Beta"), "responses_websockets") {
		t.Errorf("headers %v", hd)
	}
	// the state in the header alone, which goes on to its account only
	if gjson.GetBytes(bodies[1], "client_metadata.x-codex-turn-state").Exists() ||
		gjson.GetBytes(bodies[1], "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").String() != "true" {
		t.Errorf("client_metadata: %s", gjson.GetBytes(bodies[1], "client_metadata").Raw)
	}
}

// Only Codex's Responses route takes a socket; another of its paths is
// still told 426.
func TestCodexSocketOtherPaths(t *testing.T) {
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) { t.Error("sent to ChatGPT") })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", CodexPath+"/models", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	New().Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUpgradeRequired {
		t.Errorf("%d", rec.Code)
	}
}

// TestHeldTurnOwnsItsInput: a held reply's input was a view into the
// frame it was read from, which kept the whole frame (Codex's tools and
// instructions too) for as long as the socket held the reply. Written
// over, the frame leaves what is held as it was.
func TestHeldTurnOwnsItsInput(t *testing.T) {
	frame := []byte(`{"input":[{"type":"message","role":"user","content":"hi"},{"type":"message","role":"user","content":"again"}],"tools":["x"]}`)
	var q map[string]json.RawMessage
	if err := decodeJSONView(frame, &q); err != nil {
		t.Fatal(err)
	}
	_, fresh := prevInput(q["input"])
	prior := []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":"before"}`)}
	reply := []json.RawMessage{json.RawMessage(`{"type":"message","role":"assistant","content":"ok"}`)}
	rs := &responsesSocket{}
	held := heldTurn{items: prior, itemBytes: len(prior[0])}
	rs.keep("resp_1", held, fresh, reply, len(fresh[0])+len(fresh[1]))
	want := `[{"type":"message","role":"user","content":"before"},{"type":"message","role":"user","content":"hi"},{"type":"message","role":"user","content":"again"},{"type":"message","role":"assistant","content":"ok"}]`
	for i := range frame {
		frame[i] = 'x'
	}
	saved, ok := rs.held("resp_1")
	if got, _ := json.Marshal(saved.items); !ok || string(got) != want {
		t.Fatalf("held after the frame was written over: %s", got)
	}
	if size := len(want) - len(saved.items) - 1; saved.itemBytes != size {
		t.Fatalf("held items have %d bytes, want %d", saved.itemBytes, size)
	}
}

// A Chat upstream that has said its reply over ([DONE]) and holds the
// connection open: the turn is completed on the socket all the same, its
// usage with it, and the socket stays open for the next turn (#5936 — a
// Codex conversation stuck in progress on a Chat-only provider, the turn
// canceled by the client after the reply was whole on its side).
func TestResponsesSocketChatDoneTurn(t *testing.T) {
	fresh(t)
	f := &doneHold{}
	up := doneHoldUp(t, f)
	if err := provider.Save(provider.Provider{ID: "fake", Name: "Fake", Key: "k", Chat: up.URL + "/v1", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	_, ts := sockServer(t)
	c, _, err := dialSock(t, ts, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	turn := func(frame string) []gjson.Result {
		t.Helper()
		c.send(frame)
		got, err := c.until("response.completed", "response.failed", "response.incomplete", "error")
		if err != nil {
			t.Fatalf("%v after %v", err, got)
		}
		return got
	}
	got := turn(create("", userHi))
	if last(got).Get("type").String() != "response.completed" {
		t.Fatalf("turn 1: %v", got)
	}
	var text string
	for _, fr := range got {
		if fr.Get("type").String() == "response.output_text.delta" {
			text += fr.Get("delta").String()
		}
	}
	if text != "hello" {
		t.Errorf("turn 1 text = %q, want hello", text)
	}
	if in, out := last(got).Get("response.usage.input_tokens").Int(), last(got).Get("response.usage.output_tokens").Int(); in != 9 || out != 4 {
		t.Errorf("turn 1 usage = %d in, %d out, want 9 in and 4 out", in, out)
	}
	id := last(got).Get("response.id").String()
	got = turn(create(id, userMore))
	if last(got).Get("type").String() != "response.completed" {
		t.Fatalf("turn 2: %v", got)
	}
	if f.calls() != 2 {
		t.Errorf("%d upstream calls, want 2", f.calls())
	}
}
