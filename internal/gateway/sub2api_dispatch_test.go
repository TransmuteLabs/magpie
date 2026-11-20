package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// sub2apiNoDispatch is sub2api's 403 to /v1/messages for a key whose group
// isn't let dispatch Messages, as its handler writes it
// (openai_gateway_handler.go, anthropicErrorResponse): every Claude Code
// request of a Sidrune AI China-region key (Axo, 11-20).
const sub2apiNoDispatch = `{"type":"error","error":{"type":"permission_error","message":"This group does not allow /v1/messages dispatch"}}`

// A key whose group refuses Messages is asked its models on Chat, and
// after one refusal no model of it is asked on Messages again: the refusal
// is of the API, not of the model asked.
func TestGroupWithoutMessagesGoesToChat(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var mu sync.Mutex
	calls := map[string]int{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		calls[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, sub2apiNoDispatch)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, sse(
				`data: {"id":"c1","model":"m","choices":[{"delta":{"role":"assistant","content":"chat ok"}}]}`,
				`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
				`data: [DONE]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Key: "k", Models: []string{"claude-sonnet-4-5", "claude-opus-4-1"},
		Chat: up.URL + "/v1", Anthropic: up.URL}); err != nil {
		t.Fatal(err)
	}
	handler := New().Handler()
	for _, model := range []string{"claude-sonnet-4-5", "claude-opus-4-1", "claude-sonnet-4-5"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"relay/`+model+`","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "chat ok") {
			t.Fatalf("%s: status %d: %s", model, rec.Code, rec.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["/v1/messages"] != 1 || calls["/v1/chat/completions"] != 3 {
		t.Fatalf("Messages asked %d times, want once; chat %d, want 3 (%v)", calls["/v1/messages"], calls["/v1/chat/completions"], calls)
	}
}

// sub2api's 404 for a model no account of the key's group serves
// (no_account_error.go; Sidrune AI's gpt-5.6, Axo 11-20) rests that model
// on the key alone, the key asked again for others.
func TestSub2apiNoAccountIsTheModels(t *testing.T) {
	body := []byte(`{"error":{"type":"model_not_found","message":"Model \"gpt-5.6\" is not supported by any configured account in this group"}}`)
	if !modelRefused(http.StatusNotFound, body) {
		t.Error("not read as the model refused")
	}
}

func TestMessagesDispatchRefusalFollowsUpstreamKey(t *testing.T) {
	for _, route := range []string{"/v1/messages", "/v1/chat/completions"} {
		t.Run(route, func(t *testing.T) {
			fresh(t)
			var mu sync.Mutex
			var calls []string
			up := httptest.NewServer(withoutAnonymousFixtureProbe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := r.Header.Get("x-api-key")
				if key == "" {
					key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				}
				mu.Lock()
				calls = append(calls, key+":"+r.URL.Path)
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				switch {
				case key == "denied" && r.URL.Path == "/v1/messages":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					io.WriteString(w, sub2apiNoDispatch)
				case r.URL.Path == "/v1/chat/completions":
					io.WriteString(w, sub2apiChatReply())
				case strings.HasSuffix(r.URL.Path, "/messages"):
					io.WriteString(w, sub2apiMessagesReply())
				default:
					http.NotFound(w, r)
				}
			})))
			t.Cleanup(up.Close)
			probeFixtureRoot(t, up)
			p := provider.Provider{ID: "relay", Name: "Relay", Key: "denied", Keys: []provider.KeyAccount{{Key: "allowed"}},
				Models: []string{"claude-sonnet-4-5", "claude-opus-4-1"}, Chat: up.URL + "/v1", Anthropic: up.URL}
			if err := provider.Save(p); err != nil {
				t.Fatal(err)
			}
			for _, group := range []struct{ name, key, model string }{
				{"Denied first", "denied", "claude-sonnet-4-5"},
				{"Denied second", "denied", "claude-opus-4-1"},
				{"Allowed first", "allowed", "claude-sonnet-4-5"},
			} {
				if err := provider.SaveGroup(provider.Group{Name: group.name, Members: []string{"relay/" + group.model},
					Accounts: []string{"relay/" + provider.KeyID(group.key)}, Routing: provider.Ordered}); err != nil {
					t.Fatal(err)
				}
			}
			handler := New().Handler()
			for _, step := range []struct{ model, reply, hits string }{
				{"group/denied-first", "chat fallback", "denied:/v1/messages denied:/v1/chat/completions"},
				{"group/denied-second", "chat fallback", "denied:/v1/chat/completions"},
				{"group/allowed-first", "messages reply", "allowed:/v1/messages"},
				{"group/denied-first", "messages reply", "denied:/other/v1/messages"},
			} {
				if strings.Contains(step.hits, "/other/") {
					p.Anthropic = up.URL + "/other"
					if err := provider.Save(p); err != nil {
						t.Fatal(err)
					}
				}
				answer := sub2apiCall(handler, route, step.model)
				mu.Lock()
				hits := strings.Join(calls, " ")
				calls = nil
				mu.Unlock()
				if answer.Code != 200 || !strings.Contains(answer.Body.String(), step.reply) || hits != step.hits {
					t.Errorf("%s: status %d, hits %q want %q: %s", step.model, answer.Code, hits, step.hits, answer.Body)
				}
			}
		})
	}
}

func sub2apiCall(handler http.Handler, route, model string) *httptest.ResponseRecorder {
	reply := httptest.NewRecorder()
	body := `{"model":"` + model + `","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	handler.ServeHTTP(reply, httptest.NewRequest("POST", route, strings.NewReader(body)))
	return reply
}

func TestMessagesDispatchRefusalLeavesOnlyAPIReachable(t *testing.T) {
	fresh(t)
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, sub2apiNoDispatch)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sub2apiMessagesReply())
	}))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Key: "k", Models: []string{"claude-sonnet-4-5"}, Anthropic: up.URL}); err != nil {
		t.Fatal(err)
	}
	handler := New().Handler()
	first := sub2apiCall(handler, "/v1/messages", "relay/claude-sonnet-4-5")
	if first.Code != http.StatusForbidden {
		t.Fatalf("first call status %d: %s", first.Code, first.Body)
	}
	second := sub2apiCall(handler, "/v1/messages", "relay/claude-sonnet-4-5")
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "messages reply") || calls.Load() != 2 {
		t.Fatalf("second call status %d, %d upstream calls: %s", second.Code, calls.Load(), second.Body)
	}
}

func TestMessagesDispatchRefusalFromRedirectDoesNotMarkOriginalAPI(t *testing.T) {
	fresh(t)
	var redirected, chatCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages":
			if redirected.Load() == 0 {
				http.Redirect(w, r, "/other/v1/messages", http.StatusTemporaryRedirect)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, sub2apiMessagesReply())
		case "/other/v1/messages":
			redirected.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, sub2apiNoDispatch)
		case "/v1/chat/completions":
			chatCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, sub2apiChatReply())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Key: "k", Models: []string{"claude-sonnet-4-5"},
		Anthropic: up.URL, Chat: up.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	handler := New().Handler()
	first := sub2apiCall(handler, "/v1/messages", "relay/claude-sonnet-4-5")
	second := sub2apiCall(handler, "/v1/messages", "relay/claude-sonnet-4-5")
	if first.Code != http.StatusForbidden || second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "messages reply") ||
		redirected.Load() != 1 || chatCalls.Load() != 0 {
		t.Fatalf("first %d, second %d %s, redirect %d, chat %d", first.Code, second.Code, second.Body, redirected.Load(), chatCalls.Load())
	}
}

func sub2apiChatReply() string {
	return sse(
		`data: {"id":"c1","model":"m","choices":[{"delta":{"role":"assistant","content":"chat fallback"}}]}`,
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
		`data: [DONE]`)
}

func sub2apiMessagesReply() string {
	return sse(
		`event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}`,
		`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"messages reply"}}`,
		`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
		`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		`event: message_stop`+"\n"+`data: {"type":"message_stop"}`)
}
