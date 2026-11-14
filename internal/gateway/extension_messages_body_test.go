package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
)

func TestSDKMessagesPreservesCallerBodyThroughFront(t *testing.T) {
	id := installCanonicalExtensionFixture(t)
	server := New()
	t.Cleanup(server.idle.stop)
	handler := server.Front()
	for _, sample := range []struct {
		name, controls, messages string
	}{
		{"no thinking control", "", `[{"role":"user","content":"hello"}]`},
		{"explicit disabled", `,"thinking":{"type":"disabled"}`, `[{"role":"user","content":"hello"}]`},
		{"explicit budget", `,"thinking":{"type":"enabled","budget_tokens":1024}`, `[{"role":"user","content":"hello"}]`},
		{"explicit adaptive", `,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}`, `[{"role":"user","content":"hello"}]`},
		{"signed split tools and cache boundaries", "",
			`[{"role":"user","content":[{"type":"text","text":"read both","cache_control":{"type":"ephemeral"}}]},` +
				`{"role":"assistant","content":[{"type":"thinking","thinking":"check","signature":"opaque-origin"},` +
				`{"type":"tool_use","id":"call-a","name":"read","input":{"path":"a"},"cache_control":{"type":"ephemeral"}}]},` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"call-b","name":"read","input":{"path":"b"},"cache_control":{"type":"ephemeral","ttl":"1h"}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-a","content":"real A"}]}]`},
	} {
		t.Run(sample.name, func(t *testing.T) {
			raw := `{"model":"` + id + `/m1","max_tokens":2048,"stream":false,"messages":` +
				sample.messages + sample.controls + `}`
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(raw))
			request.RemoteAddr = "127.0.0.1:12345"
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			captured := readCanonicalExecution(t, response)
			var actual, expected map[string]any
			if err := json.Unmarshal(captured.Request.Body, &actual); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(raw), &expected); err != nil {
				t.Fatal(err)
			}
			expected["model"] = "m1"
			if !reflect.DeepEqual(actual, expected) {
				t.Errorf("SDK Messages caller body changed before execution:\ngot  %s\nwant %s", captured.Request.Body, raw)
			}
		})
	}
}

func TestSDKMessagesPreservesAdvisorCorePolicy(t *testing.T) {
	id := installCanonicalExtensionFixture(t, "claude-sonnet-5-5", "advisor-local", "m1")
	if err := provider.SetUpstreamName(id+"/advisor-local", "claude-opus-5-5"); err != nil {
		t.Fatal(err)
	}
	keys, secrets := newCaller(t, "SDK advisor caller")
	server := New()
	t.Cleanup(server.idle.stop)
	handler := server.Front()
	for _, sample := range []struct {
		name, advisor, allowed, want string
	}{
		{"restricted same provider", id + "/advisor-local", "", ""},
		{"restricted bare model", "claude-opus-5-5", "", ""},
		{"allowed same provider mapping", id + "/advisor-local[1m]", "advisor-local", "claude-opus-5-5"},
		{"allowed bare model", "claude-opus-5-5", "claude-opus-5-5", "claude-opus-5-5"},
		{"unknown other provider", "nobody/claude-opus-5-5", "", ""},
		{"unsupported bare model", "not-a-claude-model", "", ""},
	} {
		t.Run(sample.name, func(t *testing.T) {
			models := []string{id + "/claude-sonnet-5-5"}
			if sample.allowed != "" {
				models = append(models, id+"/"+sample.allowed)
			}
			if _, err := access.Update("models-key", access.Change{Key: keys[0].ID, Models: models}); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/messages",
				strings.NewReader(advisorTurn(id+"/claude-sonnet-5-5", sample.advisor)))
			request.RemoteAddr = "127.0.0.1:12345"
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+secrets[0])
			request.Header.Set(SessionHeader, "sdk-advisor-"+strings.ReplaceAll(sample.name, " ", "-"))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			captured := readCanonicalExecution(t, response)
			var body struct {
				Model    string          `json:"model"`
				Thinking json.RawMessage `json:"thinking"`
				Tools    []struct {
					Type  string `json:"type"`
					Name  string `json:"name"`
					Model string `json:"model"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(captured.Request.Body, &body); err != nil {
				t.Fatal(err)
			}
			if body.Model != "claude-sonnet-5-5" || len(body.Thinking) != 0 {
				t.Errorf("SDK executor model or caller thinking changed: %s", captured.Request.Body)
			}
			var advisors []string
			hasRead := false
			for _, tool := range body.Tools {
				if strings.HasPrefix(tool.Type, "advisor_") {
					advisors = append(advisors, tool.Model)
				}
				hasRead = hasRead || tool.Name == "Read"
			}
			if sample.want == "" && len(advisors) != 0 ||
				sample.want != "" && (len(advisors) != 1 || advisors[0] != sample.want) {
				t.Errorf("SDK advisor core policy: sent %q, want %q", advisors, sample.want)
			}
			if !hasRead {
				t.Errorf("SDK advisor policy removed the caller's Read tool: %s", captured.Request.Body)
			}
			advisorSaid(t, sample.name, string(captured.Request.Body), sample.want != "")
		})
	}
}

func TestSDKMessagesKeepsLegacyThinkingForNonSDK(t *testing.T) {
	upstream := &fake{t: t, reply: anthropicWholeOK, ctype: "application/json"}
	setup(t, provider.Anthropic, upstream)
	server := New()
	t.Cleanup(server.idle.stop)
	for _, control := range []string{"", `,"thinking":{"type":"enabled","budget_tokens":1024}`} {
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
			`{"model":"fake/m1","stream":false,"max_tokens":2048,"messages":[{"role":"user","content":"hello"}]`+control+`}`))
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Front().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("non-SDK Messages execution failed: %d %s", response.Code, response.Body)
		}
		var body struct {
			Thinking struct {
				Type   string `json:"type"`
				Budget int    `json:"budget_tokens"`
			} `json:"thinking"`
		}
		if err := json.Unmarshal(upstream.got, &body); err != nil {
			t.Fatal(err)
		}
		if control == "" && body.Thinking.Type != "disabled" ||
			control != "" && (body.Thinking.Type != "enabled" || body.Thinking.Budget != 1024) {
			t.Fatalf("non-SDK thinking behavior changed: %s", upstream.got)
		}
	}
	if upstream.calls != 2 {
		t.Fatal("non-SDK execution was retried", upstream.calls)
	}
}
