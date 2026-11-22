package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"

	builtincodex "github.com/yetone/magpie/internal/extension/builtin/codex"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func watchLinkedCodexResponses(t *testing.T, wantMode builtincodex.ResponsesWireMode) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	original := linkedCodexResponses
	linkedCodexResponses = func(req sdk.ExecuteRequest, encoding string, mode builtincodex.ResponsesWireMode, send builtincodex.ResponsesWireSend) (builtincodex.ResponsesWireReply, error) {
		hits.Add(1)
		if mode != wantMode {
			t.Errorf("linked wire mode %v, want %v", mode, wantMode)
		}
		if req.Protocol != "responses" || req.Model == "" || modelOf(req.Body) != req.Model {
			t.Errorf("invalid canonical Codex request: %q %q", req.Protocol, req.Model)
		}
		if len(req.Credentials) != 0 || len(req.Options) != 0 || len(req.Headers) != 0 {
			t.Error("linked Codex received host network or account data")
		}
		return original(req, encoding, mode, send)
	}
	t.Cleanup(func() { linkedCodexResponses = original })
	return &hits
}

func TestLinkedCodexResponsesKeepsRetryAndMetering(t *testing.T) {
	zstdTurns(t)
	codexSignedIn(t, "spare@example.com")
	hits := watchLinkedCodexResponses(t, builtincodex.ResponsesWireRoutedTurn)
	testLinkedCodexRetryAndMetering(t, hits, "acct-2", "spare@example.com", 2)
}

func TestLinkedCodexOwnRelayKeepsRetryAndMetering(t *testing.T) {
	zstdTurns(t)
	codexSignedIn(t)
	hits := watchLinkedCodexResponses(t, builtincodex.ResponsesWireRelayedTurn)
	testLinkedCodexRetryAndMetering(t, hits, "acct-1", "me@example.com", 0)
}

func TestLinkedCodexSocketSkipsHTTPEncoder(t *testing.T) {
	socketOn(t)
	zstdTurns(t)
	codexSignedIn(t)
	hits := watchLinkedCodexResponses(t, builtincodex.ResponsesWireRelayedTurn)
	watched := linkedCodexResponses
	linkedCodexResponses = func(req sdk.ExecuteRequest, encoding string, mode builtincodex.ResponsesWireMode, send builtincodex.ResponsesWireSend) (builtincodex.ResponsesWireReply, error) {
		return watched(req, encoding, mode, func(encode func() []byte) (int, int, io.ReadCloser, error) {
			if encode == nil {
				t.Fatal("turn did not supply an HTTP encoder")
			}
			return send(func() []byte {
				t.Error("successful socket turn ran the HTTP encoder")
				return encode()
			})
		})
	}
	backend := &fakeSocketBackend{frames: wholeReply}
	up := httptest.NewServer(backend)
	t.Cleanup(up.Close)
	base := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = base })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, CodexPath+"/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"ping"}`))
	req.Header.Set("Authorization", "Bearer chatgpt-token")
	req.Header.Set("chatgpt-account-id", "acct-1")
	New().Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if httpTurns, socketTurns := backend.counts(); httpTurns != 0 || socketTurns != 1 || hits.Load() != 1 {
		t.Fatalf("HTTP %d, socket %d, linked %d, want one socket through linked wire", httpTurns, socketTurns, hits.Load())
	}
}

func TestLinkedCodexSocketChoosesBeforeHTTPCompression(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		mode  builtincodex.ResponsesWireMode
		spare []string
	}{
		{name: "relay", mode: builtincodex.ResponsesWireRelayedTurn},
		{name: "routed", mode: builtincodex.ResponsesWireRoutedTurn, spare: []string{"spare@example.com"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			socketOn(t)
			codexSignedIn(t, fixture.spare...)
			t.Cleanup(provider.CodexZstdForTests(false))
			hits := watchLinkedCodexResponses(t, fixture.mode)
			backend := &fakeSocketBackend{
				whole:     true,
				closeWith: websocket.StatusMessageTooBig,
				frames: func(socketTurn) []string {
					provider.CodexZstdForTests(true)
					return nil
				},
			}
			posts := make(chan sentTurn, 2)
			up := httptest.NewServer(withoutAnonymousFixtureProbe(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodPost {
					wire, err := io.ReadAll(req.Body)
					if err != nil {
						t.Error(err)
					}
					call := sentTurn{encoding: req.Header.Get("Content-Encoding"),
						path: req.URL.Path, account: req.Header.Get("chatgpt-account-id"),
						length: req.ContentLength, wire: len(wire), body: wire}
					if call.encoding == "zstd" {
						decoder, err := zstd.NewReader(nil)
						if err != nil {
							t.Error(err)
						} else {
							call.body, err = decoder.DecodeAll(wire, nil)
							decoder.Close()
							if err != nil {
								t.Error(err)
							}
						}
					}
					select {
					case posts <- call:
					default:
						t.Error("unexpected repeated HTTP fallback")
					}
				}
				backend.ServeHTTP(w, req)
			})))
			t.Cleanup(up.Close)
			base := provider.CodexBase
			provider.CodexBase = up.URL + "/backend-api/codex"
			t.Cleanup(func() { provider.CodexBase = base })

			server := New()
			rec := httptest.NewRecorder()
			body := `{"model":"gpt-5.5","stream":true,"input":"` + strings.Repeat("ping ", 1024) + `"}`
			req := httptest.NewRequest(http.MethodPost, CodexPath+"/responses", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer chatgpt-token")
			req.Header.Set("chatgpt-account-id", "acct-1")
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if httpTurns, socketTurns := backend.counts(); httpTurns != 1 || socketTurns != 1 || hits.Load() != 1 {
				t.Fatalf("HTTP %d, socket %d, linked %d, want one of each", httpTurns, socketTurns, hits.Load())
			}
			post := <-posts
			checkZstd(t, post, "gpt-5.5")
			message := backend.turn(0).msg
			if post.account != "acct-1" || !bytes.Equal(message, codexCreate(post.body)) {
				t.Fatalf("HTTP fallback changed socket input or account: %q", post.account)
			}
			route := lastRoute(server)
			call, ok := server.RouteCall(route.ID)
			if !ok {
				t.Fatal("no call headers for the response")
			}
			headers := call.Headers()
			if headers == nil || len(headers.Upstream) != 1 || http.Header(headers.Upstream[0].Request).Get("Content-Encoding") != "zstd" {
				t.Error("call headers did not record the HTTP fallback's content encoding")
			}
			for _, row := range usage.Load(time.Time{}) {
				if row.RouteID == route.ID && !row.HandedOn {
					if want := len(message) + post.wire; row.UpBytes != want || row.PlainBytes != len(post.body) {
						t.Errorf("wire %d/%d plain %d/%d", row.UpBytes, want, row.PlainBytes, len(post.body))
					}
					return
				}
			}
			t.Fatal("no usage row for socket and HTTP fallback")
		})
	}
}

func BenchmarkLinkedCodexSocketDelta(b *testing.B) {
	wasOff := codexSocketOff.Swap(false)
	b.Cleanup(func() {
		codexKeepDrain()
		codexSocketOff.Store(wasOff)
	})
	b.Cleanup(provider.CodexZstdForTests(true))
	backend := &fakeSocketBackend{keep: true, frames: func(socketTurn) []string {
		return []string{
			`{"type":"response.created","response":{"id":"r1","model":"gpt-5.5"}}`,
			`{"type":"response.output_text.delta","delta":"pong"}`,
			`{"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":7,"output_tokens":1}}}`,
		}
	}}
	up := httptest.NewServer(backend)
	b.Cleanup(up.Close)
	base := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	b.Cleanup(func() { provider.CodexBase = base })

	handler := New().Handler()
	large := strings.Replace(userPing, "ping", strings.Repeat("text a; text b. ", 64<<10), 1)
	body := keptTurn("long-kept", "gpt-5.5", large)
	post := func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, CodexPath+"/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer chatgpt-token")
		req.Header.Set("chatgpt-account-id", "acct-1")
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
			b.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}
	post()
	b.ReportAllocs()
	for b.Loop() {
		post()
	}
	if posts, sockets := backend.counts(); posts != 0 || sockets != b.N+1 || backend.opened() != 1 {
		b.Fatalf("HTTP %d, WS turns %d, sockets %d", posts, sockets, backend.opened())
	}
	if turn := gjson.ParseBytes(backend.turn(1).msg); turn.Get("previous_response_id").String() != "r1" ||
		len(turn.Get("input").Array()) != 0 {
		b.Fatalf("kept turn was not a delta, %d bytes", len(turn.Raw))
	}
	b.ReportMetric(float64(len(backend.turn(0).msg)), "full-B")
	b.ReportMetric(float64(len(backend.turn(1).msg)), "delta-B")
}

func testLinkedCodexRetryAndMetering(t *testing.T, hits *atomic.Int32, retryAccountID, accountName string, tries int) {
	t.Helper()
	const refused = edgeUnbufferedSaid
	answer := sse(`data: {"type":"response.created","response":{"id":"codex-wire","model":"gpt-5.5"}}`,
		`data: {"type":"response.output_text.delta","delta":"pong"}`,
		`data: {"type":"response.completed","response":{"id":"codex-wire","usage":{"input_tokens":7,"output_tokens":1}}}`)
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(decoder.Close)
	var mu sync.Mutex
	var turns []sentTurn
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/backend-api/codex/responses" {
			http.NotFound(w, req)
			return
		}
		wire, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
		}
		plain, err := decoder.DecodeAll(wire, nil)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		turns = append(turns, sentTurn{path: req.URL.Path, encoding: req.Header.Get("Content-Encoding"),
			account: req.Header.Get("chatgpt-account-id"), wire: len(wire), length: req.ContentLength, body: plain})
		first := len(turns) == 1
		mu.Unlock()
		if retryAccountID == "acct-1" && req.Header.Get("Authorization") != "Bearer chatgpt-token" {
			t.Error("own relay did not pass the caller's sign-in")
		}
		if first {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusInsufficientStorage)
			io.WriteString(w, refused)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, answer)
	}))
	t.Cleanup(up.Close)
	originalBase := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = originalBase })

	server := New()
	rec := httptest.NewRecorder()
	body := `{"model":"gpt-5.5","stream":true,"input":"` + strings.Repeat("ping ", 1024) + `"}`
	req := httptest.NewRequest(http.MethodPost, CodexPath+"/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer chatgpt-token")
	req.Header.Set("chatgpt-account-id", "acct-1")
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("linked owner got %d attempts, want 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(turns) != 2 {
		t.Fatalf("vendor got %d attempts, want 2", len(turns))
	}
	for _, turn := range turns {
		checkZstd(t, turn, "gpt-5.5")
	}
	if turns[0].account != "acct-1" || turns[1].account != retryAccountID || !bytes.Equal(turns[0].body, turns[1].body) {
		t.Fatalf("retry account or body changed unexpectedly: %q -> %q", turns[0].account, turns[1].account)
	}
	route := lastRoute(server)
	var rows []usage.Record
	for _, row := range usage.Load(time.Time{}) {
		if row.RouteID == route.ID && !row.HandedOn {
			rows = append(rows, row)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("route %d has %d usage rows, want 1", route.ID, len(rows))
	}
	row := rows[0]
	if row.Tries != tries || row.UpBytes != turns[0].wire+turns[1].wire ||
		row.PlainBytes != len(turns[1].body) || row.DownBytes != len(refused)+len(answer) ||
		row.Input != 7 || row.Output != 1 || row.ProviderAccount != accountName {
		t.Fatalf("route %d usage %+v, want account %q, tries %d and vendor wire totals", route.ID, row, accountName, tries)
	}
}

func TestLinkedCodexResponsesStopsWithCaller(t *testing.T) {
	codexSignedIn(t, "spare@example.com")
	testLinkedCodexStopsWithCaller(t, watchLinkedCodexResponses(t, builtincodex.ResponsesWireRoutedTurn))
}

func TestLinkedCodexOwnRelayStopsWithCaller(t *testing.T) {
	codexSignedIn(t)
	testLinkedCodexStopsWithCaller(t, watchLinkedCodexResponses(t, builtincodex.ResponsesWireRelayedTurn))
}

func testLinkedCodexStopsWithCaller(t *testing.T, hits *atomic.Int32) {
	t.Helper()
	stopped := make(chan bool, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/backend-api/codex/responses" {
			http.NotFound(w, req)
			return
		}
		io.Copy(io.Discard, req.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`data: {"type":"response.created","response":{"id":"codex-wire-cancel","model":"gpt-5.5"}}`,
			`data: {"type":"response.output_text.delta","delta":"partial"}`))
		w.(http.Flusher).Flush()
		select {
		case <-req.Context().Done():
			stopped <- true
		case <-time.After(4 * time.Second):
			stopped <- false
		}
	}))
	t.Cleanup(up.Close)
	originalBase := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = originalBase })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	server := New()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, CodexPath+"/responses",
		strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"ping"}`))
	req.Header.Set("Authorization", "Bearer chatgpt-token")
	req.Header.Set("chatgpt-account-id", "acct-1")
	server.Handler().ServeHTTP(&cancelOnWrite{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}, req)
	select {
	case canceled := <-stopped:
		if !canceled {
			t.Error("upstream transport remained active after the client canceled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream attempt did not finish")
	}
	route := lastRoute(server)
	if hits.Load() != 1 || route.Status != usage.StatusCanceled || !route.Canceled {
		t.Fatalf("linked sends %d, status %d, canceled %v", hits.Load(), route.Status, route.Canceled)
	}
}

func TestLinkedCodexExitRelayForwardsRawReset(t *testing.T) {
	fresh(t)
	zstdTurns(t)
	hits := watchLinkedCodexResponses(t, builtincodex.ResponsesWireRelayedTurn)
	refused := edgeUnbufferedSaid + strings.Repeat(" raw", 1200)
	var requests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		if req.URL.Path != "/backend-api/codex/responses" || req.URL.RawQuery != "sample=1" ||
			req.Header.Get("Authorization") != "Bearer chatgpt-token" {
			t.Error("exit relay changed destination, query or caller sign-in")
		}
		wire, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
		}
		decoder, err := zstd.NewReader(nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer decoder.Close()
		plain, err := decoder.DecodeAll(wire, nil)
		if err != nil {
			t.Error(err)
		}
		checkZstd(t, sentTurn{path: req.URL.Path, encoding: req.Header.Get("Content-Encoding"),
			wire: len(wire), length: req.ContentLength, body: plain}, "gpt-6-luna")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Codex-Fixture", "edge")
		w.WriteHeader(http.StatusInsufficientStorage)
		io.WriteString(w, refused)
	}))
	t.Cleanup(up.Close)
	originalBase := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = originalBase })

	rec := relayDo(CodexRelay(), http.MethodPost, "localhost:3425", CodexPath+"/responses?sample=1", textTurn(8192),
		map[string]string{"Authorization": "Bearer chatgpt-token"})
	if requests.Load() != 1 || hits.Load() != 1 {
		t.Errorf("exit relay sent %d vendor requests and %d linked sends, want one each", requests.Load(), hits.Load())
	}
	if rec.Code != http.StatusInsufficientStorage || rec.Body.String() != refused ||
		rec.Header().Get("Content-Type") != "text/plain" || rec.Header().Get("X-Codex-Fixture") != "edge" {
		t.Fatalf("exit relay changed status %d, headers %v or raw body (%d bytes)", rec.Code, rec.Header(), rec.Body.Len())
	}
}

func TestLinkedCodexNativeCompactPreservesWire(t *testing.T) {
	zstdTurns(t)
	codexSignedIn(t)
	hits := watchLinkedCodexResponses(t, builtincodex.ResponsesWireNativeCompact)
	const input = `{"model":"gpt-6-luna", "input":[{"type":"message","role":"user","content":"keep this"},{ "type":"compaction_trigger" }]}`
	const output = `{"id":"resp_compact", "output":[{"type":"compaction","encrypted_content":"vendor-sealed"}]}`
	var requests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != input || r.ContentLength != int64(len(input)) || r.Header.Get("Content-Encoding") != "" {
			t.Errorf("compact request changed: %q, length %d, encoding %q, error %v", body, r.ContentLength, r.Header.Get("Content-Encoding"), err)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses/compact" ||
			r.URL.RawQuery != "sample=1" || r.Header.Get("Authorization") != "Bearer chatgpt-token" ||
			r.Header.Get("ChatGPT-Account-ID") != "acct-1" || r.Header.Get("Accept") != "application/json" {
			t.Error("compact request changed address, sign-in, account or accepted content type")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Codex-Fixture", "compact")
		io.WriteString(w, output)
	}))
	t.Cleanup(up.Close)
	originalBase := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = originalBase })
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { encoder.Close() })
	wire := encoder.EncodeAll([]byte(input), nil)

	for _, entry := range []struct {
		name    string
		handler http.Handler
	}{{"running", New().Handler()}, {"after quit", CodexRelay()}} {
		t.Run(entry.name, func(t *testing.T) {
			before := requests.Load()
			linked := hits.Load()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, CodexPath+"/responses/compact?sample=1", bytes.NewReader(wire))
			req.Host = "127.0.0.1:3425"
			req.Header.Set("Authorization", "Bearer chatgpt-token")
			req.Header.Set("ChatGPT-Account-ID", "acct-1")
			req.Header.Set("Accept", "application/json")
			req.Header.Set("Content-Encoding", "zstd")
			rec := httptest.NewRecorder()
			entry.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || rec.Body.String() != output ||
				rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("X-Codex-Fixture") != "compact" {
				t.Errorf("native compaction response changed: %d %v %q", rec.Code, rec.Header(), rec.Body)
			}
			if sent := requests.Load() - before; sent != 1 {
				t.Errorf("vendor requests = %d, want 1", sent)
			}
			if sent := hits.Load() - linked; sent != 1 {
				t.Errorf("linked native compact sends = %d, want 1", sent)
			}
		})
	}
	if rows := usage.Load(time.Time{}); len(rows) != 0 {
		t.Fatalf("native relays added %d usage rows", len(rows))
	}
}
