package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
)

type zhipuProtocolCall struct{ path, body string }
type zhipuProtocolVendor struct {
	mu             sync.Mutex
	calls          []zhipuProtocolCall
	refuseBeta     bool
	refuseMessages bool
}

func (v *zhipuProtocolVendor) RoundTrip(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	v.mu.Lock()
	v.calls = append(v.calls, zhipuProtocolCall{r.URL.Host + r.URL.Path, string(b)})
	v.mu.Unlock()
	status, ctype, reply := 200, "application/json", ""
	switch {
	case v.refuseBeta && strings.Contains(r.Header.Get("anthropic-beta"), "test-beta-2026-01-01"):
		status = 400
		reply = "{\"message\":\"Unexpected value(s) `test-beta-2026-01-01` for the `anthropic-beta` header.\"}"
	case v.refuseMessages && strings.HasSuffix(r.URL.Path, "/messages"):
		status = 400
		reply = `{"error":{"message":"use /v1/chat/completions"}}`
	case strings.HasSuffix(r.URL.Path, "/count_tokens"):
		reply = `{"input_tokens":7}`
	case strings.HasSuffix(r.URL.Path, "/messages") && gjson.GetBytes(b, "stream").Bool():
		ctype = "text/event-stream"
		reply = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"GLM-5.3\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	case strings.HasSuffix(r.URL.Path, "/messages"):
		reply = `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	case strings.HasSuffix(r.URL.Path, "/responses") && gjson.GetBytes(b, "stream").Bool():
		ctype = "text/event-stream"
		reply = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"i\",\"output_index\":0,\"content_index\":0,\"delta\":\"ok\"}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"GLM-5.3\",\"output\":[{\"type\":\"message\",\"id\":\"i\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	case strings.HasSuffix(r.URL.Path, "/responses"):
		reply = `{"id":"r","object":"response","status":"completed","model":"GLM-5.3","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
	case gjson.GetBytes(b, "stream").Bool():
		ctype = "text/event-stream"
		reply = "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"GLM-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n" +
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"GLM-5.3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"
	default:
		reply = `{"id":"c","object":"chat.completion","model":"GLM-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {ctype}}, Body: io.NopCloser(strings.NewReader(reply)), Request: r}, nil
}

func (v *zhipuProtocolVendor) take() []zhipuProtocolCall {
	v.mu.Lock()
	defer v.mu.Unlock()
	calls := v.calls
	v.calls = nil
	return calls
}

func zhipuProtocolServer(v *zhipuProtocolVendor) *Server {
	s := New()
	s.client = &http.Client{Transport: v}
	return s
}

func zhipuProtocolPost(t *testing.T, s *Server, path, body string) string {
	t.Helper()
	code, reply := postTo(t, s, path, body)
	if code != 200 {
		t.Fatalf("%s: %d %s", path, code, reply)
	}
	return reply
}

func zhipuProtocolAssertThinking(t *testing.T, body, kind, effort string, cap int) {
	t.Helper()
	th := gjson.Get(body, "thinking")
	if got := th.Get("type").String(); got != kind {
		t.Errorf("thinking.type = %q, want %q: %s", got, kind, body)
	}
	if th.Get("budget_tokens").Exists() || th.Get("display").Exists() {
		t.Errorf("native thinking retained budget/display: %s", body)
	}
	oc := gjson.Get(body, "output_config.effort")
	if got := oc.String(); got != effort || effort == "" && oc.Exists() {
		t.Errorf("effort = %q (exists %v), want %q: %s", got, oc.Exists(), effort, body)
	}
	if cap > 0 && gjson.Get(body, "max_tokens").Int() != int64(cap) {
		t.Errorf("max_tokens changed, want %d: %s", cap, body)
	}
}

func TestZhipuProtocolRoutesAndOverrides(t *testing.T) {
	fresh(t)
	v := &zhipuProtocolVendor{}
	s := zhipuProtocolServer(v)
	for _, p := range []provider.Provider{
		{ID: "plan", Name: "Plan", Key: "k", Chat: "https://api.z.ai/api/coding/paas/v4", Responses: "https://api.z.ai/api/v1", Anthropic: "https://api.z.ai/api/anthropic", Models: []string{"GLM-5.3"}},
		{ID: "payg", Name: "Payg", Key: "k", Chat: "https://api.z.ai/api/paas/v4", Anthropic: "https://api.z.ai/api/anthropic", Models: []string{"GLM-5.3"}},
		{ID: "relay", Name: "Relay", Key: "k", Chat: "https://relay.invalid/api/coding/paas/v4", Anthropic: "https://relay.invalid/api/anthropic", Models: []string{"GLM-5.3"}},
		{ID: "pinned", Name: "Pinned", Key: "k", Chat: "https://api.z.ai/api/coding/paas/v4", Responses: "https://api.z.ai/api/v1", Anthropic: "https://api.z.ai/api/anthropic", Models: []string{"GLM-5.3"}},
		{ID: "listed", Name: "Listed", Key: "k", Chat: "https://api.z.ai/api/coding/paas/v4", Anthropic: "https://api.z.ai/api/anthropic", Models: []string{"GLM-5.3"}},
	} {
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := provider.SetModelAPI("pinned/GLM-5.3", "responses"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("listed", "https://api.z.ai/api/coding/paas/v4", []catalog.Model{{ID: "GLM-5.3", APIs: []string{"chat", "anthropic"}}}); err != nil {
		t.Fatal(err)
	}
	plan, err := provider.FromPreset("zai")
	if err != nil {
		t.Fatal(err)
	}
	plan.ID, plan.Name, plan.Key, plan.KeyPlan, plan.Models = "keyplan", "Key plan", "k", "coding", []string{"GLM-5.3"}
	if err := provider.Save(plan); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, want, extra string }{
		{"plan", "/api/anthropic/v1/messages", ""},
		{"keyplan", "/api/anthropic/v1/messages", ""},
		{"payg", "/api/paas/v4/chat/completions", ""},
		{"relay", "/api/coding/paas/v4/chat/completions", ""},
		{"pinned", "/api/v1/responses", ""},
		{"listed", "/api/coding/paas/v4/chat/completions", `,"web_search_options":{}`},
	} {
		t.Run(tc.id, func(t *testing.T) {
			v.take()
			zhipuProtocolPost(t, s, "/v1/chat/completions", `{"model":"`+tc.id+`/GLM-5.3","messages":[{"role":"user","content":"hi"}]`+tc.extra+`}`)
			calls := v.take()
			if len(calls) != 1 || !strings.HasSuffix(calls[0].path, tc.want) {
				t.Fatalf("asked %+v, want %s", calls, tc.want)
			}
		})
	}
}

func TestZhipuProtocolOfficialDestinationBoundary(t *testing.T) {
	fresh(t)
	v := &zhipuProtocolVendor{}
	s := zhipuProtocolServer(v)
	for _, tc := range []struct {
		name, preset, chat, responses, anthropic, want string
	}{
		{"zai-responses", "", "", "https://api.z.ai/api/v1", "https://api.z.ai/api/anthropic", "api.z.ai/api/anthropic/v1/messages"},
		{"bigmodel-responses", "", "", "https://open.bigmodel.cn/api/v1", "https://open.bigmodel.cn/api/anthropic", "open.bigmodel.cn/api/anthropic/v1/messages"},
		{"cross-family", "", "https://api.z.ai/api/coding/paas/v4", "", "https://open.bigmodel.cn/api/anthropic", "api.z.ai/api/coding/paas/v4/chat/completions"},
		{"relay-destination", "", "https://api.z.ai/api/coding/paas/v4", "", "https://relay.invalid/api/anthropic", "api.z.ai/api/coding/paas/v4/chat/completions"},
		{"wrong-path", "", "https://api.z.ai/api/coding/paas/v4", "", "https://api.z.ai/relay", "api.z.ai/api/coding/paas/v4/chat/completions"},
		{"preset-mirror", "zai", "https://mirror.invalid/api/coding/paas/v4", "", "https://mirror.invalid/api/anthropic", "mirror.invalid/api/coding/paas/v4/chat/completions"},
		{"responses-cross-family", "", "", "https://open.bigmodel.cn/api/v1", "https://api.z.ai/api/anthropic", "open.bigmodel.cn/api/v1/responses"},
		{"responses-relay", "", "", "https://relay.invalid/api/v1", "https://relay.invalid/api/anthropic", "relay.invalid/api/v1/responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := provider.Provider{ID: tc.name, Name: tc.name, Preset: tc.preset, Key: "k", Chat: tc.chat, Responses: tc.responses, Anthropic: tc.anthropic, Models: []string{"GLM-5.3"}}
			if err := provider.Save(p); err != nil {
				t.Fatal(err)
			}
			zhipuProtocolPost(t, s, "/v1/chat/completions", `{"model":"`+tc.name+`/GLM-5.3","messages":[{"role":"user","content":"hi"}]}`)
			calls := v.take()
			if len(calls) != 1 || calls[0].path != tc.want {
				t.Fatalf("calls %+v, want %s", calls, tc.want)
			}
		})
	}
}

func TestZhipuProtocolNativeRequests(t *testing.T) {
	fresh(t)
	v := &zhipuProtocolVendor{}
	s := zhipuProtocolServer(v)
	p := provider.Provider{ID: "native", Name: "Native", Key: "k", Chat: "https://api.z.ai/api/coding/paas/v4", Responses: "https://api.z.ai/api/v1", Anthropic: "https://api.z.ai/api/anthropic", Models: []string{"GLM-5.2", "GLM-5.3", "GLM-5.3-Flash", "friendly"}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(p.ID, p.Chat, []catalog.Model{{ID: "friendly", Context: 200000, Output: 128000}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetUpstreamName("native/friendly", "GLM-5.3-Flash"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, body, kind, effort string
		cap                            int
	}{
		{"chat high tiny", "/v1/chat/completions", `{"model":"native/GLM-5.2","max_tokens":64,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`, "enabled", "high", 64},
		{"5.2 low fits high", "/v1/chat/completions", `{"model":"native/GLM-5.2","max_tokens":64,"reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`, "enabled", "high", 64},
		{"chat max 1024 streamed", "/v1/chat/completions", `{"model":"native/GLM-5.3","max_tokens":1024,"reasoning_effort":"max","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "enabled", "max", 1024},
		{"responses flash max 2048", "/v1/responses", `{"model":"native/GLM-5.3-Flash","max_output_tokens":2048,"reasoning":{"effort":"max"},"input":"hi"}`, "enabled", "max", 2048},
		{"responses off streamed", "/v1/responses", `{"model":"native/GLM-5.3","max_output_tokens":64,"reasoning":{"effort":"none"},"stream":true,"input":"hi"}`, "enabled", "low", 64},
		{"messages budget low", "/v1/messages", `{"model":"native/GLM-5.3","max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":4096,"display":"summarized"},"messages":[{"role":"user","content":"hi"}]}`, "enabled", "low", 1024},
		{"messages 5.3 off fits low", "/v1/messages", `{"model":"native/GLM-5.3","max_tokens":64,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`, "enabled", "low", 64},
		{"messages adaptive high", "/v1/messages", `{"model":"native/GLM-5.3-Flash","max_tokens":64,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`, "enabled", "high", 64},
		{"messages effort only", "/v1/messages", `{"model":"native/GLM-5.3","max_tokens":64,"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`, "enabled", "max", 64},
		{"messages effort only off", "/v1/messages", `{"model":"native/GLM-5.3","max_tokens":64,"output_config":{"effort":"none"},"messages":[{"role":"user","content":"hi"}]}`, "enabled", "low", 64},
		{"messages explicit beats budget", "/v1/messages", `{"model":"native/GLM-5.2","max_tokens":64,"thinking":{"type":"enabled","budget_tokens":4096},"output_config":{"effort":"max","format":{"type":"json_schema","schema":{"type":"object"}},"other":17},"stream":true,"messages":[{"role":"user","content":"hi"}]}`, "enabled", "max", 64},
		{"5.2 off removes stale effort", "/v1/messages", `{"model":"native/GLM-5.2","max_tokens":64,"thinking":{"type":"disabled","display":"summarized"},"output_config":{"effort":"max","other":17},"messages":[{"role":"user","content":"hi"}]}`, "disabled", "", 64},
		{"alias max without cap", "/v1/responses", `{"model":"native/friendly","reasoning":{"effort":"max"},"input":"hi"}`, "enabled", "max", 128000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v.take()
			reply := zhipuProtocolPost(t, s, tc.path, tc.body)
			if !strings.Contains(reply, "ok") {
				t.Errorf("reply lost: %s", reply)
			}
			calls := v.take()
			if len(calls) != 1 || calls[0].path != "api.z.ai/api/anthropic/v1/messages" {
				t.Fatalf("calls %+v", calls)
			}
			zhipuProtocolAssertThinking(t, calls[0].body, tc.kind, tc.effort, tc.cap)
			if strings.Contains(tc.name, "explicit beats") && (gjson.Get(calls[0].body, "output_config.format.schema.type").String() != "object" || gjson.Get(calls[0].body, "output_config.other").Int() != 17) {
				t.Errorf("output_config members lost: %s", calls[0].body)
			}
			if strings.Contains(tc.name, "alias") && gjson.Get(calls[0].body, "model").String() != "GLM-5.3-Flash" {
				t.Errorf("wire alias lost: %s", calls[0].body)
			}
		})
	}
}

func TestZhipuProtocolExtraCountAndRetry(t *testing.T) {
	fresh(t)
	v := &zhipuProtocolVendor{refuseBeta: true}
	s := zhipuProtocolServer(v)
	p := provider.Provider{ID: "extra", Name: "Extra", Key: "k", Anthropic: "https://open.bigmodel.cn/api/anthropic", Models: []string{"GLM-5.3"}, ExtraBody: provider.ExtraBody{
		"!thinking":      json.RawMessage(`{"type":"adaptive","budget_tokens":4096,"display":"summarized","custom":"kept"}`),
		"!output_config": json.RawMessage(`{"effort":"max","format":{"type":"json_schema","schema":{"type":"object"}},"other":17}`),
		"custom_field":   json.RawMessage(`true`),
	}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"extra/GLM-5.3","max_tokens":64,"thinking":{"type":"enabled","budget_tokens":24000},"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("anthropic-beta", "test-beta-2026-01-01")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
	calls := v.take()
	if len(calls) != 2 {
		t.Fatalf("expected retry, got %+v", calls)
	}
	for _, c := range calls {
		zhipuProtocolAssertThinking(t, c.body, "enabled", "max", 64)
		if gjson.Get(c.body, "thinking.custom").String() != "kept" || gjson.Get(c.body, "output_config.other").Int() != 17 || !gjson.Get(c.body, "custom_field").Bool() || gjson.Get(c.body, "output_config.format.schema.type").String() != "object" {
			t.Errorf("extra fields lost: %s", c.body)
		}
	}
	route := lastRoute(s)
	if len(route.Tries) != 1 || route.Tries[0].Effort != "max" || lastUsage(t).Effort != "max" {
		t.Fatalf("forced native effort missing from route or usage: %+v", route.Tries)
	}
	zhipuProtocolPost(t, s, "/v1/messages/count_tokens", body)
	calls = v.take()
	if len(calls) != 1 || !strings.HasSuffix(calls[0].path, "/v1/messages/count_tokens") {
		t.Fatalf("count calls %+v", calls)
	}
	zhipuProtocolAssertThinking(t, calls[0].body, "enabled", "high", 64)
}

func TestZhipuProtocolEffortPreferencesAndFallback(t *testing.T) {
	fresh(t)
	v := &zhipuProtocolVendor{}
	s := zhipuProtocolServer(v)
	p := provider.Provider{ID: "prefs", Name: "Preferences", Key: "k", Chat: "https://api.z.ai/api/coding/paas/v4", Anthropic: "https://api.z.ai/api/anthropic", Models: []string{"restricted", "stale"}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(p.ID, p.Chat, []catalog.Model{{ID: "restricted"}, {ID: "stale", Efforts: []string{"none"}}}); err != nil {
		t.Fatal(err)
	}
	for _, model := range p.Models {
		if err := provider.SetUpstreamName(p.ID+"/"+model, "GLM-5.3"); err != nil {
			t.Fatal(err)
		}
	}
	if err := provider.SetModelEfforts("prefs/restricted", []string{"high"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, body, effort string }{
		{"/v1/chat/completions", `{"model":"prefs/restricted","max_tokens":64,"reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`, "high"},
		{"/v1/messages", `{"model":"prefs/restricted","max_tokens":64,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`, "high"},
		{"/v1/messages", `{"model":"prefs/restricted","max_tokens":64,"thinking":{"type":"enabled","budget_tokens":4096},"messages":[{"role":"user","content":"hi"}]}`, "high"},
		{"/v1/messages", `{"model":"prefs/restricted","max_tokens":64,"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`, "high"},
		{"/v1/messages/count_tokens", `{"model":"prefs/restricted","max_tokens":64,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`, "high"},
		{"/v1/messages/count_tokens", `{"model":"prefs/restricted","max_tokens":64,"thinking":{"type":"enabled","budget_tokens":4096},"messages":[{"role":"user","content":"hi"}]}`, "high"},
		{"/v1/messages/count_tokens", `{"model":"prefs/restricted","max_tokens":64,"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`, "high"},
		{"/v1/messages", `{"model":"prefs/stale","max_tokens":64,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`, "low"},
		{"/v1/chat/completions", `{"model":"prefs/stale","max_tokens":64,"reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`, "max"},
	} {
		zhipuProtocolPost(t, s, tc.path, tc.body)
		calls := v.take()
		if len(calls) != 1 {
			t.Fatalf("calls %+v", calls)
		}
		zhipuProtocolAssertThinking(t, calls[0].body, "enabled", tc.effort, 64)
	}

	fallback := provider.Provider{ID: "fallback", Name: "Fallback", Key: "k", Chat: "https://api.z.ai/api/coding/paas/v4", Anthropic: "https://api.z.ai/api/anthropic", Models: []string{"small"}}
	if err := provider.Save(fallback); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(fallback.ID, fallback.Chat, []catalog.Model{{ID: "small", Efforts: []string{"low", "high"}}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetUpstreamName("fallback/small", "GLM-5.3"); err != nil {
		t.Fatal(err)
	}
	v.refuseMessages = true
	zhipuProtocolPost(t, s, "/v1/responses", `{"model":"fallback/small","max_output_tokens":64,"reasoning":{"effort":"xhigh"},"input":"hi"}`)
	calls := v.take()
	if len(calls) != 2 || !strings.HasSuffix(calls[0].path, "/v1/messages") || !strings.HasSuffix(calls[1].path, "/chat/completions") {
		t.Fatalf("endpoint fallback calls %+v", calls)
	}
	zhipuProtocolAssertThinking(t, calls[0].body, "enabled", "max", 64)
	if got := gjson.Get(calls[1].body, "reasoning_effort").String(); got != "high" {
		t.Errorf("Chat fallback effort %q, want high: %s", got, calls[1].body)
	}
}

func TestZhipuProtocolSelectedEffortsAtTinyCap(t *testing.T) {
	fresh(t)
	v := &zhipuProtocolVendor{}
	s := zhipuProtocolServer(v)
	p := provider.Provider{ID: "selected", Name: "Selected", Key: "k", Chat: "https://api.z.ai/api/coding/paas/v4", Anthropic: "https://api.z.ai/api/anthropic", Models: []string{"friendly"}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(p.ID, p.Chat, []catalog.Model{{ID: "friendly", Efforts: []string{"low", "high", "max"}}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetUpstreamName("selected/friendly", "GLM-5.3"); err != nil {
		t.Fatal(err)
	}
	fixed := provider.Group{ID: "zfixed", Name: "Z Fixed", Members: []string{"selected/friendly:high"}, Routing: provider.Ordered}
	ruled := provider.Group{ID: "zruled", Name: "Z Ruled", Members: []string{"selected/friendly"}, Routing: provider.Ordered,
		Rules: []provider.Rule{{Use: "selected/friendly", Tokens: 1, UseEffort: "high"}}}
	mapped := provider.Group{ID: "zmapped", Name: "Z Mapped", Members: []string{"selected/friendly"}, Routing: provider.Ordered}
	mapped.SetMemberEffortMap("selected/friendly", map[string]string{"none": "high", "low": "max"})
	bounded := provider.Group{ID: "zbounded", Name: "Z Bounded", Members: []string{"selected/friendly"}, Routing: provider.Ordered, Levels: []string{"high", "max"}}
	for _, g := range []provider.Group{fixed, ruled, mapped, bounded} {
		if err := provider.SaveGroup(g); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, group, thinking, want, fixed, rule, mapped string
	}{
		{"fixed off", "zfixed", `"thinking":{"type":"disabled","custom":"kept"}`, "high", "high", "", ""},
		{"rule off", "zruled", `"thinking":{"type":"disabled","custom":"kept"}`, "high", "", "high", ""},
		{"mapped off", "zmapped", `"thinking":{"type":"disabled","custom":"kept"}`, "high", "", "", "none"},
		{"mapped enabled", "zmapped", `"thinking":{"type":"enabled","budget_tokens":4096,"custom":"kept"}`, "max", "", "", "low"},
		{"bounded enabled", "zbounded", `"thinking":{"type":"enabled","budget_tokens":4096,"custom":"kept"}`, "high", "", "", ""},
		{"bounded off stays off", "zbounded", `"thinking":{"type":"disabled","custom":"kept"}`, "low", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"group/%s","max_tokens":64,%s,"output_config":{"other":17},"messages":[{"role":"user","content":%q}]}`,
				tc.group, tc.thinking, strings.Repeat("routing ", 100))
			route := postProto(t, s, "/v1/messages", body)
			calls := v.take()
			if len(calls) != 1 || calls[0].path != "api.z.ai/api/anthropic/v1/messages" {
				t.Fatalf("calls %+v", calls)
			}
			zhipuProtocolAssertThinking(t, calls[0].body, "enabled", tc.want, 64)
			if gjson.Get(calls[0].body, "model").String() != "GLM-5.3" || gjson.Get(calls[0].body, "thinking.custom").String() != "kept" || gjson.Get(calls[0].body, "output_config.other").Int() != 17 {
				t.Errorf("alias or extra fields lost: %s", calls[0].body)
			}
			if len(route.Tries) != 1 || route.Tries[0].Fixed != tc.fixed || route.Tries[0].RuleEffort != tc.rule || route.Tries[0].MappedFrom != tc.mapped || route.Tries[0].Effort != tc.want {
				t.Fatalf("route %+v", route.Tries)
			}
			if tc.rule != "" && (route.Rule == nil || route.Rule.UseEffort != tc.rule) {
				t.Fatalf("rule did not select effort: %+v", route.Rule)
			}
		})
	}
}

func TestZhipuProtocolTurnPickAtTinyCap(t *testing.T) {
	s, _, _, classifier := jevved(t, provider.EffortAuto)
	classifier.score = 2.4
	v := &zhipuProtocolVendor{}
	s.client = &http.Client{Transport: countTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "api.z.ai" {
			return v.RoundTrip(r)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	p := provider.Provider{ID: "turnpick", Name: "Turn Pick", Key: "k", Anthropic: "https://api.z.ai/api/anthropic", Models: []string{"friendly"}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(p.ID, p.Anthropic, []catalog.Model{{ID: "friendly", Efforts: []string{"low", "high", "max"}}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetUpstreamName("turnpick/friendly", "GLM-5.3"); err != nil {
		t.Fatal(err)
	}
	setMembers(t, []string{"turnpick/friendly"})
	for _, tc := range []struct {
		thinking, want string
		picked         bool
	}{
		{`{"type":"enabled","budget_tokens":4096}`, "high", true},
		{`{"type":"disabled"}`, "low", false},
	} {
		body := `{"model":"group/r","max_tokens":64,"thinking":` + tc.thinking + `,"messages":[{"role":"user","content":"redesign the scheduler"}]}`
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		req.Header.Set(SessionHeader, tc.want)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", tc.thinking, rec.Code, rec.Body)
		}
		calls := v.take()
		if len(calls) != 1 {
			t.Fatalf("%s: calls %+v", tc.thinking, calls)
		}
		zhipuProtocolAssertThinking(t, calls[0].body, "enabled", tc.want, 64)
		route := lastRoute(s)
		if len(route.Tries) != 1 || route.Tries[0].Picked != tc.picked || route.Tries[0].Effort != tc.want {
			t.Fatalf("%s: tries %+v", tc.thinking, route.Tries)
		}
	}
}

func TestZhipuProtocolDesktopSubagentTinyCount(t *testing.T) {
	fresh(t)
	v := &zhipuProtocolVendor{}
	s := zhipuProtocolServer(v)
	p := provider.Provider{ID: "worker", Name: "Worker", Key: "k", Anthropic: "https://api.z.ai/api/anthropic", Models: []string{"friendly"}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetUpstreamName("worker/friendly", "GLM-5.3"); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetDesktopSubagentEffort("high"); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"worker/friendly","max_tokens":64,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"task"}],"tools":[{"name":"Bash","input_schema":{"type":"object"}}]}`
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("x-api-key", Token+"-claude-desktop")
		req.Header.Set("User-Agent", desktopCodeUA)
		req.Header.Set("x-claude-code-session-id", "lead-session")
		req.Header.Set("x-claude-code-agent-id", "worker")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		calls := v.take()
		if len(calls) != 1 || !strings.HasSuffix(calls[0].path, path) || gjson.Get(calls[0].body, "model").String() != "GLM-5.3" {
			t.Fatalf("%s: calls %+v", path, calls)
		}
		zhipuProtocolAssertThinking(t, calls[0].body, "enabled", "high", 64)
	}
}

func TestZhipuProtocolWireEffortAccounting(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, effort string
	}{
		{"translated 5.2 low", "/v1/chat/completions", `{"model":"accounting/GLM-5.2","max_tokens":64,"reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`, "high"},
		{"relayed 5.3 off", "/v1/messages", `{"model":"accounting/GLM-5.3","max_tokens":64,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`, "low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			v := &zhipuProtocolVendor{refuseBeta: true}
			s := zhipuProtocolServer(v)
			p := provider.Provider{ID: "accounting", Name: "Accounting", Key: "k", Chat: "https://api.z.ai/api/coding/paas/v4", Anthropic: "https://api.z.ai/api/anthropic", Models: []string{"GLM-5.2", "GLM-5.3"}}
			if err := provider.Save(p); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
			req.Header.Set("anthropic-beta", "test-beta-2026-01-01")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			calls := v.take()
			if len(calls) != 2 {
				t.Fatalf("expected native retry, got %+v", calls)
			}
			for _, call := range calls {
				zhipuProtocolAssertThinking(t, call.body, "enabled", tc.effort, 64)
			}
			route := lastRoute(s)
			if len(route.Tries) != 1 || route.Tries[0].Effort != tc.effort {
				t.Fatalf("route tries %+v, want %s", route.Tries, tc.effort)
			}
			if got := lastUsage(t).Effort; got != tc.effort {
				t.Fatalf("usage effort %q, want %q", got, tc.effort)
			}
		})
	}
}

func TestZhipuProtocolForeignRelayAndUnsignedReasoning(t *testing.T) {
	fresh(t)
	v := &zhipuProtocolVendor{}
	s := zhipuProtocolServer(v)
	for _, p := range []provider.Provider{
		{ID: "native", Name: "Native", Key: "k", Chat: "https://api.z.ai/api/coding/paas/v4", Anthropic: "https://api.z.ai/api/anthropic", Models: []string{"GLM-5.3"}},
		{ID: "relay", Name: "Relay", Key: "k", Anthropic: "https://relay.invalid/api/anthropic", Models: []string{"GLM-5.3"}},
	} {
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"model":"native/GLM-5.3","reasoning_effort":"high","messages":[{"role":"user","content":"hi"},{"role":"assistant","reasoning_content":"provided reasoning","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"done"}]}`
	zhipuProtocolPost(t, s, "/v1/chat/completions", body)
	calls := v.take()
	if len(calls) != 1 || !strings.Contains(calls[0].body, "provided reasoning") {
		t.Fatalf("unsigned reasoning lost: %+v", calls)
	}
	body = strings.Replace(body, `"reasoning_content":"provided reasoning",`, "", 1)
	zhipuProtocolPost(t, s, "/v1/chat/completions", body)
	calls = v.take()
	if len(calls) != 1 || strings.Contains(calls[0].body, `"type":"thinking"`) {
		t.Fatalf("unprovided tool-turn reasoning invented: %+v", calls)
	}
	zhipuProtocolPost(t, s, "/v1/messages", `{"model":"relay/GLM-5.3","max_tokens":64,"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`)
	calls = v.take()
	if len(calls) != 1 || gjson.Get(calls[0].body, "thinking.type").String() != "adaptive" || gjson.Get(calls[0].body, "thinking.display").String() != "summarized" {
		t.Fatalf("third-party relay changed: %+v", calls)
	}
	// A display ID resembling ZCode is not a validated plugin or native host.
	p := provider.Provider{ID: "zcode", Key: "k", Anthropic: "https://relay.invalid/api/anthropic"}
	res, err := s.forwardOnce(context.Background(), p, provider.Anthropic, "/v1/messages", []byte(`{"model":"GLM-5.3","thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"max"}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	calls = v.take()
	if len(calls) != 1 || gjson.Get(calls[0].body, "thinking.type").String() != "adaptive" {
		t.Fatalf("provider ID spoofed native: %+v", calls)
	}
}

func TestZhipuProtocolPluginOwner(t *testing.T) {
	up := &adaptiveVendor{}
	intrinsicTrialFixture(t, false, false, up.ServeHTTP)
	pid := "zcode"
	body := fmt.Sprintf(`{"model":%q,"max_tokens":64,"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`, pid+"/GLM-5.3")
	zhipuProtocolPost(t, New(), "/v1/messages", body)
	b, _ := json.Marshal(up.last())
	zhipuProtocolAssertThinking(t, string(b), "enabled", "max", 64)
	up.mu.Lock()
	before := len(up.bodies)
	up.mu.Unlock()
	reply := zhipuProtocolPost(t, New(), "/v1/messages/count_tokens", body)
	count := gjson.Get(reply, "input_tokens")
	if count.Type != gjson.Number || count.Float() < 0 {
		t.Fatalf("local count: %s", reply)
	}
	up.mu.Lock()
	after := len(up.bodies)
	up.mu.Unlock()
	if after != before {
		t.Fatalf("ZCode plugin was asked to count: %d calls before, %d after", before, after)
	}
}
