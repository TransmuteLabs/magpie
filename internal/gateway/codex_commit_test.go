package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func inferenceOnlyFixture(test *testing.T, endpoint, model string, next http.Handler) http.Handler {
	test.Helper()
	modelsAt := strings.TrimSuffix(endpoint, "/chat/completions")
	if modelsAt == endpoint {
		modelsAt = endpoint[:strings.LastIndex(endpoint, "/")]
	}
	modelsAt += "/models"
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == modelsAt {
			test.Logf("fixture serves discovery %s %s from %s (user-agent %q; body bytes %d)",
				request.Method, request.URL.Path, request.RemoteAddr, request.UserAgent(), request.ContentLength)
			response.Header().Set("Content-Type", "application/json")
			if strings.HasPrefix(endpoint, "/backend-api/codex/") {
				json.NewEncoder(response).Encode(map[string]any{"models": []map[string]string{{"slug": model, "display_name": model}}})
			} else {
				json.NewEncoder(response).Encode(map[string]any{"object": "list", "data": []map[string]string{{"id": model, "object": "model"}}})
			}
			return
		}
		if request.Method != http.MethodPost || request.URL.Path != endpoint {
			test.Logf("fixture ignores %s %s from %s (user-agent %q; body bytes %d)",
				request.Method, request.URL.Path, request.RemoteAddr, request.UserAgent(), request.ContentLength)
			http.NotFound(response, request)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, (1<<20)+1))
		request.Body.Close()
		var input struct {
			Model string `json:"model"`
		}
		if err != nil || len(body) > 1<<20 || json.Unmarshal(body, &input) != nil || input.Model != model {
			http.Error(response, "invalid inference fixture request", http.StatusBadRequest)
			return
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(response, request)
	})
}

func setupCodexChatFixture(test *testing.T, upstream *fake) *httptest.Server {
	test.Helper()
	noRests()
	test.Cleanup(noRests)
	test.Setenv("XDG_CONFIG_HOME", test.TempDir())
	test.Setenv("XDG_CACHE_HOME", test.TempDir())
	server := httptest.NewServer(inferenceOnlyFixture(test, "/v1/chat/completions", "m1", upstream))
	test.Cleanup(server.Close)
	if err := provider.Save(provider.Provider{ID: "fake", Name: "Fake", Key: "k", Models: []string{"m1"}, Chat: server.URL + "/v1"}); err != nil {
		test.Fatal(err)
	}
	return server
}

func probeInferenceFixture(test *testing.T, base, modelsAt string) {
	test.Helper()
	for _, endpoint := range []string{"/", modelsAt} {
		request, err := http.NewRequest(http.MethodGet, base+endpoint, nil)
		if err != nil {
			test.Fatal(err)
		}
		request.Header.Set("User-Agent", "magpie-fixture-regression")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			test.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if err != nil {
			test.Fatal(err)
		}
	}
}

func TestInferenceFixtureCountsOnlyIntendedRequests(test *testing.T) {
	for _, fixture := range []struct{ endpoint, modelsAt, body string }{
		{"/v1/chat/completions", "/v1/models", `{"model":"fixture-model","messages":[{"role":"user","content":"hi"}]}`},
		{"/backend-api/codex/responses", "/backend-api/codex/models", `{"model":"fixture-model","input":"hi","stream":true}`},
		{"/v1/messages", "/v1/models", `{"model":"fixture-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		test.Run(fixture.endpoint, func(test *testing.T) {
			var calls atomic.Int32
			handler := inferenceOnlyFixture(test, fixture.endpoint, "fixture-model", http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				got, err := io.ReadAll(request.Body)
				if err != nil || string(got) != fixture.body {
					test.Errorf("guard changed the inference body: %q %v", got, err)
				}
				io.WriteString(response, `{"ok":true}`)
			}))
			for _, input := range []struct {
				method string
				path   string
				body   string
				status int
			}{
				{http.MethodGet, "/", "", http.StatusNotFound},
				{http.MethodGet, "/", "", http.StatusNotFound},
				{http.MethodPost, "/other", fixture.body, http.StatusNotFound},
				{http.MethodPost, fixture.endpoint, "", http.StatusBadRequest},
				{http.MethodPost, fixture.endpoint, `{"model":"other"}`, http.StatusBadRequest},
				{http.MethodPost, fixture.endpoint, `{"model":2}`, http.StatusBadRequest},
			} {
				request := httptest.NewRequest(input.method, input.path, strings.NewReader(input.body))
				request.Header.Set("User-Agent", "magpie-fixture-regression")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != input.status || calls.Load() != 0 {
					test.Fatalf("unrelated %s %s consumed inference state: status=%d calls=%d", input.method, input.path, response.Code, calls.Load())
				}
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fixture.modelsAt, nil))
			var catalog struct {
				Data   []struct{ ID string }   `json:"data"`
				Models []struct{ Slug string } `json:"models"`
			}
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &catalog) != nil || calls.Load() != 0 ||
				len(catalog.Data) == 0 && len(catalog.Models) == 0 ||
				len(catalog.Data) != 0 && catalog.Data[0].ID != "fixture-model" ||
				len(catalog.Models) != 0 && catalog.Models[0].Slug != "fixture-model" {
				test.Fatalf("model discovery was not an isolated valid catalog: %d %s calls=%d", response.Code, response.Body.String(), calls.Load())
			}
			response = httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, fixture.endpoint, strings.NewReader(fixture.body)))
			if response.Code != http.StatusOK || response.Body.String() != `{"ok":true}` || calls.Load() != 1 {
				test.Fatalf("valid inference did not reach its fixture once: %d %s calls=%d", response.Code, response.Body.String(), calls.Load())
			}
		})
	}
}

const (
	commitSchema = `{"type":"object","properties":{"message":{"type":"string"}},"required":["message"],"additionalProperties":false}`
	prSchema     = `{"type":"object","properties":{"title":{"type":"string","maxLength":120},"body":{"type":"string"}},"required":["title","body"],"additionalProperties":false}`
)

// The Codex app's "generate commit message" and pull request text are
// ephemeral turns whose answer must be the JSON its schema asks for
// ({"message"}, {"title","body"}). A model behind the Chat API never sees
// the schema and answers in plain text, which the app turned down (makenxis
// on X), so magpie hands the text back in the schema's shape.
func TestCodexCommitMessageOnMagpieProvider(t *testing.T) {
	for _, c := range []struct{ source, schema, said, want string }{
		{"commit_message", commitSchema, `Fix login redirect loop`, `{"message":"Fix login redirect loop"}`},
		{"commit_message", commitSchema, "```\\nFix login redirect loop\\n\\nKeep the next URL.\\n```", `{"message":"Fix login redirect loop\n\nKeep the next URL."}`},
		{"commit_message", commitSchema, `{\"message\":\"Own JSON\"}`, `{"message":"Own JSON"}`},
		{"pull_request_message", prSchema, "Fix login loop\\n\\n## Summary\\n- redirect", `{"body":"## Summary\n- redirect","title":"Fix login loop"}`},
	} {
		f := &fake{t: t, reply: sse(
			`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"`+c.said+`"}}]}`,
			`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
			`data: [DONE]`)}
		upstream := setupCodexChatFixture(t, f)
		probeInferenceFixture(t, upstream.URL, "/v1/models")
		s := New()
		body := `{"model":"fake/m1","stream":true,"input":"Using the supplied git context below, generate a git commit message.","text":{"format":{"type":"json_schema","schema":` + c.schema + `}}}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
		req.Header.Set("x-codex-turn-metadata", `{"thread_source":"`+c.source+`"}`)
		s.Handler().ServeHTTP(rec, req)
		text, _ := outputText(t, rec.Body.String())
		if rec.Code != 200 || text != c.want || f.calls != 1 {
			t.Errorf("%s %q: %d %q, want %q, calls %d", c.source, c.said, rec.Code, text, c.want, f.calls)
		}
	}
}

func TestCommitJSON(t *testing.T) {
	var both titleShape
	json.Unmarshal([]byte(`{"properties":{"message":{"type":"string"},"title":{"type":"string","maxLength":10},"body":{"type":"string"}},"required":["message","title","body"]}`), &both)
	long := strings.Repeat("x", 80)
	for _, c := range []struct {
		in    string
		shape titleShape
		want  string
	}{
		{"Add tests", titleShape{}, `{"message":"Add tests"}`},
		{"message: Add tests", titleShape{}, `{"message":"Add tests"}`},
		{long + "\n\nbody", titleShape{}, `{"message":"` + long[:72] + `\n\nbody"}`},
		{"Add the tests\n\nlong body", both, `{"body":"long body","message":"Add the tests\n\nlong body","title":"Add the te"}`},
		{"", titleShape{}, ""},
		{"{broken", titleShape{}, ""},
	} {
		if got := commitJSON(c.in, c.shape); got != c.want {
			t.Errorf("commitJSON(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
