package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

// keepTurn runs one Responses request through previousResponse, answered
// with output, and returns the turn kept under id.
func keepTurn(t *testing.T, body, id, output string) *prevTurn {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	w, r, _, done := previousResponse(httptest.NewRecorder(), r, provider.Responses, []byte(body))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	io.WriteString(w, `{"id":"`+id+`","object":"response","status":"completed","output":[`+output+`]}`)
	done()
	k, ok := prevResponses.get(prevKey{prevScope(r), id})
	if !ok {
		t.Fatalf("%s not kept", id)
	}
	return k
}

// TestResentHistorySharesKeptBytes: a client that sends its whole
// conversation each turn, naming no earlier response, has the history a
// kept turn holds shared, not copied again, and counted once for as long
// as any turn holding it is kept.
func TestResentHistorySharesKeptBytes(t *testing.T) {
	old := prevResponses
	prevResponses = newPrevStore(prevCap, prevTTL)
	t.Cleanup(func() { prevResponses = old })
	u1 := `{"type":"message","role":"user","content":"` + strings.Repeat("long question ", 2000) + `"}`
	a1 := `{"type":"message","role":"assistant","content":"answer one"}`
	u2 := `{"type":"message","role":"user","content":"and then?"}`
	first := keepTurn(t, `{"model":"m","input":[`+u1+`]}`, "resp_1", a1)
	second := keepTurn(t, `{"model":"m","input":[`+u1+`,`+a1+`,`+u2+`]}`, "resp_2", `{"type":"message","role":"assistant","content":"two"}`)
	if got, _ := json.Marshal(second.items); !strings.HasPrefix(string(got), `[`+u1+`,`+a1+`,`+u2+`,`) {
		t.Fatalf("kept %.200s", got)
	}
	for i := range 2 {
		if unsafe.SliceData(second.items[i]) != unsafe.SliceData(first.items[i]) {
			t.Fatalf("item %d copied again", i)
		}
	}
	if want := len(u2) + len(`{"type":"message","role":"assistant","content":"two"}`); second.own != want {
		t.Fatalf("own %d, want the %d bytes the turn added", second.own, want)
	}
	both := first.own + second.own
	if prevResponses.size != both {
		t.Fatalf("size %d, want %d", prevResponses.size, both)
	}
	prevResponses.mu.Lock()
	prevResponses.drop(prevResponses.m[first.key])
	prevResponses.mu.Unlock()
	// the second still holds u1 and a1: they stay counted
	if prevResponses.size != both {
		t.Fatalf("size %d once the first went, want %d: what the second holds is uncounted", prevResponses.size, both)
	}
	prevResponses.mu.Lock()
	prevResponses.drop(prevResponses.m[second.key])
	prevResponses.mu.Unlock()
	if prevResponses.size != 0 {
		t.Fatalf("size %d with nothing kept", prevResponses.size)
	}
}

func TestResponsesSocketShortReplayDropsExpiredSuffix(test *testing.T) {
	fresh(test)
	previousStore := prevResponses
	store := newPrevStore(prevCap, prevTTL)
	const firstSecond = 100
	var clock atomic.Int64
	clock.Store(firstSecond)
	store.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	prevResponses = store
	test.Cleanup(func() { prevResponses = previousStore })

	const firstID, shortID = "resp_prefix_full", "resp_prefix_short"
	const replyItem = `{"type":"message","id":"msg_prefix_full","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"old answer"}]}`
	const completed = `{"type":"response.completed","response":{"id":"` + firstID + `","object":"response","status":"completed","output":[` + replyItem + `],"usage":{"input_tokens":7,"output_tokens":1,"total_tokens":8}}}`
	const incomplete = `{"type":"response.incomplete","response":{"id":"` + shortID + `","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":11,"output_tokens":16,"total_tokens":27}}}`
	upstream := &respUp{reply: func(writer http.ResponseWriter, _ *http.Request, attempt int) {
		writer.Header().Set("Content-Type", "text/event-stream")
		switch attempt {
		case 1:
			io.WriteString(writer, sse(
				`data: {"type":"response.created","response":{"id":"`+firstID+`","object":"response","status":"in_progress","output":[]}}`,
				`data: {"type":"response.output_item.done","output_index":0,"item":`+replyItem+`}`,
				`data: `+completed,
			))
		case 2:
			clock.Store(firstSecond + 2)
			io.WriteString(writer, sse(
				`data: {"type":"response.created","response":{"id":"`+shortID+`","object":"response","status":"in_progress","output":[]}}`,
				`data: `+incomplete,
			))
		default:
			http.Error(writer, "unexpected retry", http.StatusInternalServerError)
		}
	}}
	sockUp(test, upstream)
	server, front := sockServer(test)
	client, _, err := dialSock(test, front, "/v1/responses", nil)
	if err != nil {
		test.Fatal(err)
	}
	const input = `[` + userHi + `]`
	const frame = `{"type":"response.create","model":"fake/m1","store":true,"max_output_tokens":16,"input":` + input + `}`
	for _, want := range []string{completed, incomplete} {
		if terminal := last(client.turn(frame)).Raw; terminal != want {
			test.Fatalf("terminal %s, want %s", terminal, want)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	closed := make(chan error, 1)
	go func() {
		_, _, readErr := client.c.Read(ctx)
		closed <- readErr
	}()
	server.sockets.drain(ctx)
	if ctx.Err() != nil {
		test.Fatal("socket drain timed out")
	}
	if err := <-closed; websocket.CloseStatus(err) != websocket.StatusGoingAway {
		test.Fatalf("socket close: %v", err)
	}
	if count := upstream.calls(); count != 2 {
		test.Fatalf("%d upstream calls, want 2", count)
	}
	for index := range 2 {
		request := gjson.ParseBytes(upstream.body(index))
		if request.Get("input").Raw != input || request.Get("previous_response_id").Exists() {
			test.Fatalf("upstream request %d: %s", index+1, request.Raw)
		}
	}
	rows := usage.Load(time.Time{})
	if len(rows) != 2 {
		test.Fatalf("got %d Usage rows, want 2", len(rows))
	}
	for index, want := range [][2]int{{7, 1}, {11, 16}} {
		row := rows[index]
		if row.Status != http.StatusOK || row.Input != want[0] || row.Output != want[1] {
			test.Fatalf("Usage row %d: %+v, want 200 and %v tokens", index+1, row, want)
		}
	}

	fullKey, shortKey := prevKey{"key:", firstID}, prevKey{"key:", shortID}
	full, ok := store.get(fullKey)
	if !ok || len(full.items) != 2 || len(full.chunks) != 2 || string(full.items[1]) != replyItem {
		test.Fatalf("full reply missing its separate output: %+v", full)
	}
	clock.Store(firstSecond + int64(prevTTL/time.Second) + 1)
	if _, ok := store.get(fullKey); ok {
		test.Fatal("full reply remained available after expiry")
	}
	short, ok := store.get(shortKey)
	if !ok || len(short.items) != 1 || string(short.items[0]) != userHi || store.size != len(userHi) {
		test.Fatalf("short reply does not hold exactly its prefix: %+v, %d bytes", short, store.size)
	}
	if unsafe.SliceData(short.items[0]) != unsafe.SliceData(full.items[0]) {
		test.Fatal("the matching prefix bytes were copied")
	}
	if unsafe.SliceData(short.items) == unsafe.SliceData(full.items) {
		test.Fatal("the short reply retains the expired reply's item array and output")
	}
}

// TestPrevStoreCountsHeldBytes: over random turns sharing, following and
// pushing each other out, the store's size is always the bytes of the
// chunks its turns hold, each once, and every turn its own input and
// output.
func TestPrevStoreCountsHeldBytes(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	st := newPrevStore(4000, prevTTL)
	var convs [][]json.RawMessage
	var ids, scopes []string
	shared := 0
	for i := range 3000 {
		var fresh []json.RawMessage
		var prior *prevTurn
		// mostly one of the newest conversations goes on, as clients do
		c := len(convs)
		if n := len(convs); n > 0 && r.IntN(8) != 0 {
			c = n - 1 - r.IntN(min(n, 3))
		}
		if c == len(convs) {
			scopes = append(scopes, fmt.Sprint("s", r.IntN(2)))
		}
		scope := scopes[c]
		switch {
		case c < len(convs) && r.IntN(4) == 0 && ids[c] != "":
			// names the earlier turn
			if k, ok := st.get(prevKey{scope, ids[c]}); ok {
				prior = k
			}
			fresh = []json.RawMessage{json.RawMessage(fmt.Sprintf(`"n%d"`, i))}
		case c < len(convs):
			// sends it whole again, and something new
			fresh = append(convs[c][:len(convs[c]):len(convs[c])], json.RawMessage(fmt.Sprintf(`"m%d%s"`, i, strings.Repeat("x", r.IntN(200)))))
		default:
			fresh = []json.RawMessage{json.RawMessage(fmt.Sprintf(`"f%d"`, i))}
			convs, ids = append(convs, nil), append(ids, "")
		}
		k, _ := st.kept(scope, prior, fresh)
		if k.own < len(fresh[len(fresh)-1]) {
			t.Fatalf("own %d under the new item", k.own)
		}
		total := 0
		for _, it := range fresh {
			total += len(it)
		}
		if prior == nil && k.own < total {
			shared++
		}
		out := []json.RawMessage{json.RawMessage(fmt.Sprintf(`"o%d"`, i))}
		id := fmt.Sprint("r", i)
		turn := &prevTurn{key: prevKey{scope, id}, items: k.items, own: k.own, chunks: k.chunks, ends: k.ends}
		turn.add(out)
		want := fresh
		if prior != nil {
			want = append(prior.items[:len(prior.items):len(prior.items)], fresh...)
		}
		want = append(want[:len(want):len(want)], out...)
		if g, w := fmt.Sprint(turn.items), fmt.Sprint(want); g != w {
			t.Fatalf("turn %d holds %.200s, want %.200s", i, g, w)
		}
		if len(turn.ends) != len(turn.chunks) || turn.ends[len(turn.ends)-1] != len(turn.items) {
			t.Fatalf("turn %d: ends %v over %d items", i, turn.ends, len(turn.items))
		}
		st.put(turn)
		convs[c], ids[c] = want, id
		// the size is the distinct chunks held
		held := map[*prevChunk]bool{}
		sum := 0
		for e := st.order.Front(); e != nil; e = e.Next() {
			for _, ch := range e.Value.(*prevTurn).chunks {
				if !held[ch] {
					held[ch] = true
					sum += ch.size
				}
				if ch.refs <= 0 {
					t.Fatalf("a held chunk has refs %d", ch.refs)
				}
			}
		}
		if st.size != sum || st.size > st.cap {
			t.Fatalf("after turn %d: size %d, chunks held %d, cap %d", i, st.size, sum, st.cap)
		}
	}
	if shared < 500 {
		t.Fatalf("only %d turns shared: the test reaches too little", shared)
	}
}
