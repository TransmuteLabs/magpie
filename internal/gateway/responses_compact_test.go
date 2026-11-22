package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/requestpolicy"
	"github.com/yetone/magpie/internal/settings"
)

// #7281 (jinpwng): Pi's Responses compaction extension asks POST
// /v1/responses/compact of a provider set to the Responses API. magpie
// answered it the 404 of an unknown path and never asked the upstream.
// From the generic /v1 base URL, and from the bare one, a key's provider
// on Responses is asked once, at /responses/compact under its base, and
// its unary compaction object comes back as it came, the sealed item
// untouched; the next /responses carrying it goes back there (#1099).
func TestResponsesCompactRelayed(t *testing.T) {
	const reply = `{"id":"resp_c1","object":"response.compaction","created_at":1,"output":[{"type":"compaction","id":"cmp_1","encrypted_content":"gAAAAopaque=="}],"usage":{"input_tokens":120,"output_tokens":30,"total_tokens":150}}`
	for _, path := range []string{"/v1/responses/compact", "/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			f := &fake{t: t, ctype: "application/json", reply: reply}
			setup(t, provider.Responses, f)
			if err := provider.SetNativeCompaction("fake", true); err != nil {
				t.Fatal(err)
			}
			// an earlier compaction in the input stays the client's: not
			// magpie's summary path (handle's codexCompact)
			code, body := post(t, path, `{"model":"fake/m1","instructions":"summarise",
			  "input":[{"type":"compaction","encrypted_content":"gAAAAearlier=="},{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]}]}`)
			if code != 200 {
				t.Fatalf("%d %s", code, body)
			}
			if f.calls != 1 || f.path != "/v1/responses/compact" {
				t.Fatalf("upstream asked %d times, at %q", f.calls, f.path)
			}
			var got map[string]any
			if err := json.Unmarshal(f.got, &got); err != nil {
				t.Fatalf("upstream got %s", f.got)
			}
			if got["model"] != "m1" || !strings.Contains(string(f.got), "gAAAAearlier==") || strings.Contains(string(f.got), `"stream":true`) {
				t.Errorf("upstream got %s", f.got)
			}
			var out struct {
				Object string `json:"object"`
				Output []struct {
					Type string `json:"type"`
					Enc  string `json:"encrypted_content"`
				} `json:"output"`
			}
			if err := json.Unmarshal([]byte(body), &out); err != nil || out.Object != "response.compaction" || len(out.Output) != 1 || out.Output[0].Enc != "gAAAAopaque==" {
				t.Fatalf("client got %s", body)
			}
			// who sealed it is remembered, for the next /responses
			cp, ok := checkpointOf([]byte(`{"input":[{"type":"compaction","encrypted_content":"gAAAAopaque=="}]}`))
			if !ok || cp.Provider != "fake" {
				t.Errorf("sealed by %+v, %v", cp, ok)
			}
		})
	}
}

func TestResponsesCompactRefusesOpaqueHistoryOffResponses(t *testing.T) {
	for _, path := range []string{"/v1/responses/compact", "/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			f := &fake{t: t, ctype: "application/json", reply: `{}`}
			setup(t, provider.Chat, f)
			code, body := post(t, path, `{"model":"fake/m1","input":[{"type":"compaction","encrypted_content":"gAAAAopaque=="},{"type":"message","role":"user","content":"hi"}]}`)
			if code != 400 || !strings.Contains(body, "unreadable checkpoints") || !strings.Contains(body, "invalid_request_error") {
				t.Fatalf("%d %s", code, body)
			}
			if f.calls != 0 {
				t.Errorf("upstream asked %d times: %s", f.calls, f.got)
			}
		})
	}
}

func TestResponsesCompactKeepsMagpieCheckpointLocal(t *testing.T) {
	const checkpoint = `{"type":"compaction","encrypted_content":"magpie1:UHJldmlvdXMgc3VtbWFyeQ=="}`
	for _, tc := range []struct{ name, input string }{
		{"ordinary", `"input":[` + checkpoint + `,{"role":"user","content":"new history"}]`},
		{"last type", `"input":[{"type":"message",` + checkpoint[1:] + `,{"role":"user","content":"new history"}]`},
		{"last seal", `"input":[{"encrypted_content":"vendor-checkpoint",` + checkpoint[1:] + `,{"role":"user","content":"new history"}]`},
		{"last input", `"input":"old input","input":[` + checkpoint + `,{"role":"user","content":"new history"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fake{t: t, reply: deepseekSummary(provider.Responses)}
			setup(t, provider.Responses, f)
			if err := provider.SetNativeCompaction("fake", true); err != nil {
				t.Fatal(err)
			}
			code, body := post(t, "/v1/responses/compact", `{"model":"fake/m1",`+tc.input+`}`)
			if code != 200 || !strings.Contains(body, "magpie1:") ||
				f.calls != 1 || f.path != "/v1/responses" || !strings.Contains(string(f.got), "Previous summary") {
				t.Fatalf("checkpoint was not decoded locally: status=%d calls=%d path=%q upstream=%s response=%s", code, f.calls, f.path, f.got, body)
			}
		})
	}
}

func compactSealedStream(enc string) string {
	item := `{"type":"compaction","id":"cmp_7285","encrypted_content":"` + enc + `"}`
	return sse(
		`data: {"type":"response.created","response":{"id":"resp_7285","object":"response","created_at":1795000000,"status":"in_progress","output":[]}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"compaction","id":"cmp_7285"}}`,
		`data: {"type":"response.compaction.compacting","output_index":0}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
		`data: {"type":"response.completed","response":{"id":"resp_7285","object":"response","created_at":1795000000,"status":"completed","output":[`+item+`],"usage":{"input_tokens":120,"output_tokens":30,"total_tokens":150}}}`)
}

const compactAsked = `{"model":"%s","instructions":"summarise","input":[
  {"type":"compaction","encrypted_content":"gAAAAearlier=="},
  {"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]},
  {"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},
  {"type":"message","role":"user","content":[{"type":"input_text","text":"now the tests"}]}]}`

func checkCompaction(t *testing.T, body, enc string) {
	t.Helper()
	var out struct {
		Object string `json:"object"`
		Output []struct {
			Type string `json:"type"`
			Role string `json:"role"`
			Enc  string `json:"encrypted_content"`
		} `json:"output"`
		Usage struct {
			Total int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Object != "response.compaction" || len(out.Output) != 3 ||
		out.Output[0].Role != "user" || out.Output[1].Role != "user" || out.Output[2].Type != "compaction" || out.Output[2].Enc != enc || out.Usage.Total != 150 {
		t.Fatalf("client got %s", body)
	}
}

func TestResponsesCompactChatGPTAccount(t *testing.T) {
	for _, path := range []string{"/v1/responses/compact", "/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			codexSignedIn(t)
			var mu sync.Mutex
			var asked []string
			up := httptest.NewServer(withoutAnonymousFixtureProbe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				asked = append(asked, r.URL.Path+" "+string(b))
				mu.Unlock()
				if r.URL.Path != "/backend-api/codex/responses" {
					w.WriteHeader(404)
					io.WriteString(w, `{"detail":"Not Found"}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, compactSealedStream("gAAAAB-sealed-by-chatgpt"))
			})))
			t.Cleanup(up.Close)
			was := provider.CodexBase
			provider.CodexBase = up.URL + "/backend-api/codex"
			t.Cleanup(func() { provider.CodexBase = was })

			nativeOn(t, "codex")
			code, body := post(t, path, strings.Replace(compactAsked, "%s", "codex/gpt-5.4", 1))
			if code != 200 {
				t.Fatalf("%d %s", code, body)
			}
			checkCompaction(t, body, "gAAAAB-sealed-by-chatgpt")
			mu.Lock()
			defer mu.Unlock()
			if len(asked) != 1 || !strings.HasPrefix(asked[0], "/backend-api/codex/responses ") {
				t.Fatalf("backend asked %q", asked)
			}
			got := strings.TrimPrefix(asked[0], "/backend-api/codex/responses ")
			var req struct {
				Stream bool              `json:"stream"`
				Input  []json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal([]byte(got), &req); err != nil || !req.Stream || len(req.Input) == 0 ||
				string(req.Input[len(req.Input)-1]) != `{"type":"compaction_trigger"}` || !strings.Contains(got, "gAAAAearlier==") {
				t.Errorf("backend got %s", got)
			}
			cp, ok := checkpointOf([]byte(`{"input":[{"type":"compaction","encrypted_content":"gAAAAB-sealed-by-chatgpt"}]}`))
			if !ok || cp.Provider != "codex" {
				t.Errorf("sealed by %+v, %v", cp, ok)
			}
		})
	}
}

func TestResponsesCompactKeepsLastInputRoleAndType(t *testing.T) {
	codexSignedIn(t)
	const sealed = "gAAAAB-compaction-input-fixture"
	var calls atomic.Int32
	up := httptest.NewServer(withoutAnonymousFixtureProbe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		input, err := io.ReadAll(r.Body)
		if err != nil || !strings.Contains(string(input), `"type":"compaction_trigger"`) {
			t.Errorf("ChatGPT compaction request: %s (%v)", input, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, compactSealedStream(sealed))
	})))
	t.Cleanup(up.Close)
	was := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = was })
	nativeOn(t, "codex")

	code, body := post(t, "/v1/responses/compact", `{"model":"codex/gpt-5.4","input":[
		{"type":"message","role":"user","role":"assistant","content":"first role is user"},
		{"type":"message","role":"assistant","role":"user","content":"last role is user"},
		{"role":"user","type":"message","type":"reasoning","content":"first type is message"},
		{"role":"user","type":"reasoning","type":"message","content":"last type is message"},
		{"role":"user","type":"message","type":null,"content":"last type is null"},
		{"role":"user","type":null,"type":"message","content":"first type is null"},
		{"role":"user","type":null,"content":"explicit null type"},
		{"role":"user","content":"omitted type"}
	]}`)
	if calls.Load() != 1 || code != http.StatusOK {
		t.Fatalf("upstream calls = %d, got HTTP %d: %s", calls.Load(), code, body)
	}
	type item struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content string `json:"content"`
		Sealed  string `json:"encrypted_content"`
	}
	var out struct {
		Object string `json:"object"`
		Output []item `json:"output"`
	}
	want := []item{
		{Type: "message", Role: "user", Content: "last role is user"},
		{Type: "message", Role: "user", Content: "last type is message"},
		{Type: "message", Role: "user", Content: "first type is null"},
		{Role: "user", Content: "omitted type"},
		{Type: "compaction", Sealed: sealed},
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Object != "response.compaction" || !slices.Equal(out.Output, want) {
		t.Fatalf("got %s (%v), want output %+v", body, err, want)
	}
}

func TestResponsesCompactCopilotAccount(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	copilotFake(t, "gho_compact7285", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/models":
			io.WriteString(w, `{"data":[{"id":"gpt-5.3-codex","name":"GPT-5.3-Codex","model_picker_enabled":true,"policy":{"state":"enabled"},"supported_endpoints":["/responses"],"capabilities":{"type":"chat"}}]}`)
			return
		}
		mu.Lock()
		asked = append(asked, r.URL.Path+" "+string(b))
		mu.Unlock()
		if r.URL.Path != "/responses" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, compactSealedStream("gAAAAB-sealed-by-copilot"))
	})
	nativeOn(t, "copilot")
	code, body := post(t, "/v1/responses/compact", strings.Replace(compactAsked, "%s", "copilot/gpt-5.3-codex", 1))
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	checkCompaction(t, body, "gAAAAB-sealed-by-copilot")
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || !strings.HasPrefix(asked[0], "/responses ") || !strings.Contains(asked[0], `{"type":"compaction_trigger"}`) {
		t.Errorf("Copilot was asked %q", asked)
	}
}

func TestResponsesCompactAccountWithoutCompaction(t *testing.T) {
	codexSignedIn(t)
	up := httptest.NewServer(withoutAnonymousFixtureProbe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"SUMMARY"}]}}`,
			`data: {"type":"response.completed","response":{"id":"resp_s","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
	})))
	t.Cleanup(up.Close)
	was := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = was })
	nativeOn(t, "codex")
	code, body := post(t, "/v1/responses/compact", strings.Replace(compactAsked, "%s", "codex/gpt-5.4", 1))
	if code != 502 || !strings.Contains(body, "without a compaction item") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestResponsesCompactRequiresOutputArray(t *testing.T) {
	const sealed = "gAAAAB-compaction-array-fixture"
	const item = `{"type":"compaction","id":"cmp_array","encrypted_content":"` + sealed + `"}`
	const object = `,"output":{"0":` + item + `}`
	const array = `,"output":[` + item + `]`
	const emptySealLast = `,"output":[{"type":"compaction","encrypted_content":"` + sealed + `","encrypted_content":""}]`
	const nonemptySealLast = `,"output":[{"type":"compaction","encrypted_content":"","encrypted_content":"` + sealed + `"}]`
	for _, test := range []struct {
		name, output            string
		stream, done, truncated bool
		want                    int
	}{
		{name: "json-array", output: array, want: http.StatusOK},
		{name: "json-object", output: object, want: http.StatusBadGateway},
		{name: "json-truncated", output: array, truncated: true, want: http.StatusBadGateway},
		{name: "json-object-after-array", output: array + object, want: http.StatusBadGateway},
		{name: "json-empty-seal-last", output: emptySealLast, want: http.StatusBadGateway},
		{name: "json-nonempty-seal-last", output: nonemptySealLast, want: http.StatusOK},
		{name: "sse-array", output: array, stream: true, want: http.StatusOK},
		{name: "sse-object", output: object, stream: true, want: http.StatusBadGateway},
		{name: "sse-object-with-item", output: object, stream: true, done: true, want: http.StatusBadGateway},
		{name: "sse-object-after-array-with-item", output: array + object, stream: true, done: true, want: http.StatusBadGateway},
		{name: "sse-null-with-item", output: `,"output":null`, stream: true, done: true, want: http.StatusBadGateway},
		{name: "sse-missing-with-item", stream: true, done: true, want: http.StatusBadGateway},
		{name: "sse-empty-array-with-item", output: `,"output":[]`, stream: true, done: true, want: http.StatusOK},
		{name: "sse-empty-seal-last", output: emptySealLast, stream: true, want: http.StatusBadGateway},
		{name: "sse-nonempty-seal-last", output: nonemptySealLast, stream: true, want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			codexSignedIn(t)
			reply := `{"id":"resp_array","status":"completed","usage":{"input_tokens":120,"output_tokens":30,"total_tokens":150}` + test.output + `}`
			if test.truncated {
				reply = strings.TrimSuffix(reply, "}")
			}
			contentType := "application/json"
			if test.stream {
				contentType = "text/event-stream"
				reply = sse(`data: {"type":"response.completed","response":` + reply + `}`)
				if test.done {
					reply = sse(`data: {"type":"response.output_item.done","output_index":0,"item":`+item+`}`) + reply
				}
			}
			var calls atomic.Int32
			up := httptest.NewServer(withoutAnonymousFixtureProbe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" {
					http.NotFound(w, r)
					return
				}
				calls.Add(1)
				input, err := io.ReadAll(r.Body)
				if err != nil || !strings.Contains(string(input), `"type":"compaction_trigger"`) {
					t.Errorf("ChatGPT compaction request: %s (%v)", input, err)
				}
				w.Header().Set("Content-Type", contentType)
				io.WriteString(w, reply)
			})))
			t.Cleanup(up.Close)
			was := provider.CodexBase
			provider.CodexBase = up.URL + "/backend-api/codex"
			t.Cleanup(func() { provider.CodexBase = was })
			nativeOn(t, "codex")

			retries := 0
			current := settings.Load()
			current.RequestPolicy = &requestpolicy.Config{Defaults: requestpolicy.Options{MaxTotalRetries: &retries}}
			if err := settings.Save(current); err != nil {
				t.Fatal(err)
			}
			code, body := post(t, "/v1/responses/compact", strings.Replace(compactAsked, "%s", "codex/gpt-5.4", 1))
			if calls.Load() != 1 || code != test.want {
				t.Fatalf("upstream calls = %d, got HTTP %d, want HTTP %d: %s", calls.Load(), code, test.want, body)
			}
			if code == http.StatusOK {
				checkCompaction(t, body, sealed)
			} else if !strings.Contains(body, "without a compaction item") || strings.Contains(body, sealed) {
				t.Fatalf("client got %s", body)
			}
		})
	}
}
