package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

type normalizationRequest struct {
	method, path string
	body         []byte
	headers      http.Header
}

type normalizationRequests struct {
	mu    sync.Mutex
	items []normalizationRequest
}

func (requests *normalizationRequests) record(t *testing.T, request *http.Request) {
	t.Helper()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Error(err)
	}
	requests.mu.Lock()
	defer requests.mu.Unlock()
	requests.items = append(requests.items, normalizationRequest{
		method: request.Method, path: request.URL.RequestURI(), body: body, headers: request.Header.Clone(),
	})
}

func (requests *normalizationRequests) take() []normalizationRequest {
	requests.mu.Lock()
	defer requests.mu.Unlock()
	items := requests.items
	requests.items = nil
	return items
}

func normalizationServer(t *testing.T, answer http.HandlerFunc) (*Server, *normalizationRequests) {
	t.Helper()
	fresh(t)
	requests := &normalizationRequests{}
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if anonymousFixtureRoot(request.Method, request.URL.RequestURI(), request.Header, request.ContentLength != 0 || len(request.TransferEncoding) != 0) {
			http.NotFound(writer, request)
			return
		}
		requests.record(t, request)
		answer(writer, request)
	}))
	t.Cleanup(upstream.Close)
	probeFixtureRoot(t, upstream)
	if err := provider.Save(provider.Provider{
		ID: "proofchat", Name: "Proof Chat", Key: "proof-provider-secret", Chat: upstream.URL + "/v1", Models: []string{"m1"},
		Headers: map[string]string{"X-Route": "fixed-route"}, PassHeaders: []string{"X-Tenant", "X-Route"},
	}); err != nil {
		t.Fatal(err)
	}
	server := New()
	t.Cleanup(server.idle.stop)
	return server, requests
}

func normalizationJSON(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	io.WriteString(writer, `{"id":"proof","model":"m1","choices":[{"message":{"role":"assistant","content":"normalized"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
}

func normalizationFront(server *Server, path, body, secret string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "normalization-fixture")
	request.Header.Set("X-Tenant", "tenant-from-client")
	request.Header.Set("X-Route", "client-route")
	request.Header.Set("X-Session-Id", "normalization-session")
	if secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	response := httptest.NewRecorder()
	server.Front().ServeHTTP(response, request)
	return response
}

func TestBuiltinChatNormalizationThroughFront(t *testing.T) {
	for _, scenario := range []struct {
		name, body string
		check      func(*testing.T, []byte)
	}{
		{
			name: "effort_cache_and_message_tools",
			body: `{"model":"proofchat/m1","thinking":{"type":"enabled","effort":"high"},"promptCacheKey":"camel-key","tools":[{"name":"keep","description":"first","parameters":{"type":"object"}}],"messages":[{"role":"system","tools":[{"name":"keep","description":"later","inputSchema":{"type":"object"}},{"name":"late","inputSchema":{"type":"object"}}]},{"role":"user","content":"hello"}],"unknown_number":1.50e2}`,
			check: func(t *testing.T, body []byte) {
				if gjson.GetBytes(body, "reasoning_effort").String() != "high" || gjson.GetBytes(body, "thinking.effort").Exists() {
					t.Errorf("nested effort was not normalized: %s", body)
				}
				if gjson.GetBytes(body, "prompt_cache_key").String() != "camel-key" || gjson.GetBytes(body, "promptCacheKey").Exists() {
					t.Errorf("camel cache key was not normalized: %s", body)
				}
				tools := gjson.GetBytes(body, "tools").Array()
				if len(tools) != 2 || tools[0].Get("function.name").String() != "keep" ||
					tools[0].Get("function.description").String() != "first" ||
					tools[1].Get("type").String() != "function" || tools[1].Get("function.name").String() != "late" ||
					tools[1].Get("function.parameters.type").String() != "object" {
					t.Errorf("message tools were not lifted/shaped in first-definition order: %s", body)
				}
				if len(gjson.GetBytes(body, "messages").Array()) != 1 || gjson.GetBytes(body, "messages.0.tools").Exists() {
					t.Errorf("tool-only system message survived normalization: %s", body)
				}
			},
		},
		{
			name: "explicit_values_win",
			body: `{"model":"proofchat/m1","reasoning_effort":"low","thinking":{"type":"enabled","effort":"high"},"promptCacheKey":"camel-key","prompt_cache_key":"own-key","messages":[{"role":"user","content":"hello"}],"unknown_number":1.50e2}`,
			check: func(t *testing.T, body []byte) {
				if gjson.GetBytes(body, "reasoning_effort").String() != "low" ||
					gjson.GetBytes(body, "prompt_cache_key").String() != "own-key" || gjson.GetBytes(body, "promptCacheKey").Exists() {
					t.Errorf("explicit effort/cache precedence changed: %s", body)
				}
			},
		},
		{
			name: "already_canonical",
			body: `{"model":"proofchat/m1","prompt_cache_key":"own-key","tools":[{"type":"function","function":{"name":"ready","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"hello <b> & 漢字"}],"unknown_number":1.50e2}`,
			check: func(t *testing.T, body []byte) {
				if gjson.GetBytes(body, "tools.0.function.name").String() != "ready" ||
					gjson.GetBytes(body, "messages.0.content").String() != "hello <b> & 漢字" ||
					gjson.GetBytes(body, "prompt_cache_key").String() != "own-key" {
					t.Errorf("canonical fields changed: %s", body)
				}
			},
		},
	} {
		for _, path := range []string{"/v1/chat/completions", "/chat/completions"} {
			t.Run(scenario.name+path, func(t *testing.T) {
				server, requests := normalizationServer(t, normalizationJSON)
				response := normalizationFront(server, path, scenario.body, "")
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "normalized") {
					t.Fatalf("ordinary Chat response: %d %s", response.Code, response.Body)
				}
				heard := requests.take()
				if len(heard) != 1 || heard[0].method != http.MethodPost || heard[0].path != "/v1/chat/completions" {
					t.Fatalf("direct Chat dispatch: %+v", heard)
				}
				scenario.check(t, heard[0].body)
				if gjson.GetBytes(heard[0].body, "unknown_number").Float() != 150 {
					t.Errorf("unknown numeric field changed: %s", heard[0].body)
				}
				if heard[0].headers.Get("Authorization") != "Bearer proof-provider-secret" ||
					heard[0].headers.Get("X-Tenant") != "tenant-from-client" || heard[0].headers.Get("X-Route") != "client-route" {
					t.Errorf("provider auth or configured headers changed: %v", heard[0].headers)
				}
				records := usage.Load(time.Time{})
				if len(records) != 1 || records[0].Input != 7 || records[0].Output != 2 || len(server.Recent()) != 1 {
					t.Fatalf("ordinary accounting: recent=%d records=%+v", len(server.Recent()), records)
				}
			})
		}
	}
}

func TestBuiltinChatNormalizationStreamCancelAccounting(t *testing.T) {
	for _, outcome := range []string{"complete", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			release := make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			upstreamDone := make(chan struct{})
			server, requests := normalizationServer(t, func(writer http.ResponseWriter, request *http.Request) {
				defer close(upstreamDone)
				writer.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
				writer.(http.Flusher).Flush()
				select {
				case <-release:
					io.WriteString(writer, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
				case <-request.Context().Done():
				}
			})
			observer := &fakeObserver{watch: true}
			t.Cleanup(usage.Observe(observer))
			keys, secrets := newCaller(t, "Stream normalization")
			keyCap(t, keys[0].ID, 1)
			frontDone := make(chan struct{})
			front := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if anonymousFixtureRoot(request.Method, request.URL.RequestURI(), request.Header, request.ContentLength != 0 || len(request.TransferEncoding) != 0) {
					http.NotFound(writer, request)
					return
				}
				defer close(frontDone)
				server.Front().ServeHTTP(writer, request)
			}))
			t.Cleanup(front.Close)
			probeFixtureRoot(t, front)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			t.Cleanup(func() {
				cancel()
				releaseOnce()
				front.CloseClientConnections()
			})
			heldBefore := roomHeld(bodyRoom)
			body := `{"model":"proofchat/m1","stream":true,"promptCacheKey":"stream-cache","messages":[{"role":"user","content":"hello"}],"padding":"` + strings.Repeat("x", int(bodyRoomFree)) + `"}`
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+secrets[0])
			response, err := front.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { response.Body.Close() })
			reader := bufio.NewReader(response.Body)
			var received strings.Builder
			for !strings.Contains(received.String(), `"content":"first"`) {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatalf("first event did not flush before completion: %v", err)
				}
				received.WriteString(line)
			}
			select {
			case <-upstreamDone:
				t.Fatal("upstream completed before its held first event was observed")
			default:
			}
			if outcome == "canceled" {
				cancel()
				response.Body.Close()
			} else {
				releaseOnce()
				tail, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				received.Write(tail)
				response.Body.Close()
				if strings.Count(received.String(), "data: [DONE]") != 1 {
					t.Fatalf("stream completion changed: %s", received.String())
				}
			}
			for _, joined := range []<-chan struct{}{upstreamDone, frontDone} {
				select {
				case <-joined:
				case <-time.After(5 * time.Second):
					t.Fatal("stream handler or upstream did not join")
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for roomHeld(bodyRoom) != heldBefore {
				if time.Now().After(deadline) {
					t.Fatal("completed stream retained request body room")
				}
				time.Sleep(time.Millisecond)
			}
			if len(server.KeyLanes()) != 0 {
				t.Fatal("completed stream retained a key slot")
			}
			heard := requests.take()
			if len(heard) != 1 || gjson.GetBytes(heard[0].body, "prompt_cache_key").String() != "stream-cache" {
				t.Fatal("stream normalized incorrectly or dispatched more than once")
			}
			records := usage.Load(time.Time{})
			trace := server.Trace(t.Context(), 0, 0)
			if len(records) != 1 || len(server.Recent()) != 1 || len(trace.Routes) != 1 || !trace.Routes[0].Done {
				t.Fatalf("stream accounting duplicated or missing: records=%d recent=%d routes=%+v", len(records), len(server.Recent()), trace.Routes)
			}
			if outcome == "canceled" {
				if !usage.CanceledBy(records[0].Status, records[0].Error) || records[0].Failed() {
					t.Fatalf("caller cancellation attribution: %+v", records[0])
				}
			} else if records[0].Input != 7 || records[0].Output != 2 || records[0].Failed() {
				t.Fatalf("stream usage: %+v", records[0])
			}
			observer.mu.Lock()
			defer observer.mu.Unlock()
			if len(observer.begun) != 1 || len(observer.attempts) != 1 || len(observer.ends) != 1 || len(observer.rows) != 1 {
				t.Fatalf("observer lifecycle: begin=%d attempts=%d end=%d ledger=%d",
					len(observer.begun), len(observer.attempts), len(observer.ends), len(observer.rows))
			}
		})
	}
}

func TestBuiltinChatNormalizationBoundsHeadersAndRights(t *testing.T) {
	for _, scenario := range []string{"above_native_limit", "configured_limit", "read_only", "model_held", "remote_unkeyed", "untrusted_origin"} {
		t.Run(scenario, func(t *testing.T) {
			server, requests := normalizationServer(t, normalizationJSON)
			keys, secrets := newCaller(t, "Normalization rights")
			body := `{"model":"proofchat/m1","messages":[{"role":"user","content":"hello"}]}`
			want := http.StatusOK
			if scenario == "above_native_limit" {
				body = strings.TrimSuffix(body, "}") + `,"padding":"` + strings.Repeat("x", (16<<20)+1) + `"}`
			}
			if scenario == "configured_limit" {
				server.requestLimits.body = 16
				want = http.StatusRequestEntityTooLarge
			}
			if scenario == "read_only" {
				if _, err := access.Update("access-key", access.Change{Key: keys[0].ID, ReadOnly: true}); err != nil {
					t.Fatal(err)
				}
				want = http.StatusForbidden
			}
			if scenario == "model_held" {
				if _, err := access.Update("models-key", access.Change{Key: keys[0].ID, Models: []string{"other/*"}}); err != nil {
					t.Fatal(err)
				}
				want = http.StatusForbidden
			}
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/chat/completions", strings.NewReader(body))
			request.RemoteAddr = "127.0.0.1:12345"
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+secrets[0])
			request.Header.Set("X-Tenant", "tenant-from-client")
			if scenario == "remote_unkeyed" {
				request.RemoteAddr = "192.0.2.20:12345"
				request.Header.Del("Authorization")
				want = http.StatusUnauthorized
			}
			if scenario == "untrusted_origin" {
				request.Header.Set("Origin", "https://untrusted.example")
				request.Header.Del("Authorization")
				want = http.StatusForbidden
			}
			response := httptest.NewRecorder()
			server.Front().ServeHTTP(response, request)
			heard := requests.take()
			if response.Code != want {
				t.Fatalf("rights/bounds response=%d want=%d: %s", response.Code, want, response.Body)
			}
			if want >= 400 {
				if len(heard) != 0 {
					t.Fatalf("denied ordinary Chat dispatched %d times", len(heard))
				}
				return
			}
			if len(heard) != 1 || len(gjson.GetBytes(heard[0].body, "padding").String()) != (16<<20)+1 ||
				heard[0].headers.Get("X-Tenant") != "tenant-from-client" ||
				heard[0].headers.Get("Authorization") != "Bearer proof-provider-secret" {
				t.Fatal("ordinary body cap, PassHeaders or credential ownership changed")
			}
			headers, err := json.Marshal(server.Recent()[0].Headers())
			if err != nil || strings.Contains(string(headers), "proof-provider-secret") || strings.Contains(string(headers), secrets[0]) {
				t.Fatal("public header observation exposed a transport credential")
			}
		})
	}
}
