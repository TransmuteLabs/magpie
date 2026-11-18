package gateway

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

func TestStandaloneResponsesCompactReplaysItsCheckpoint(t *testing.T) {
	upstream := &fake{t: t, reply: sse(
		`data: {"type":"response.created","response":{"id":"resp_compact","status":"in_progress","output":[]}}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"CHECKPOINT SUMMARY"}]}}`,
		`data: {"type":"response.completed","response":{"id":"resp_compact","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}`)}
	setup(t, provider.Responses, upstream)
	server := New().Handler()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{
		"model":"fake/m1",
		"input":[{"type":"message","role":"developer","content":"LEADING INSTRUCTIONS"},{"type":"message","role":"user","content":"ORIGINAL HISTORY"}],
		"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]
	}`))
	server.ServeHTTP(response, request)
	if response.Code != 200 || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("standalone compaction returned %d %s: %s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	var result struct {
		Object string `json:"object"`
		Output []struct {
			Type      string `json:"type"`
			Encrypted string `json:"encrypted_content"`
		} `json:"output"`
		Usage struct {
			Total int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Object != "response.compaction" || len(result.Output) != 1 || result.Output[0].Type != "compaction" || result.Usage.Total != 12 {
		t.Fatalf("wrong response envelope: %s", response.Body.String())
	}
	if body := string(upstream.got); upstream.calls != 1 || upstream.path != "/v1/responses" ||
		!strings.Contains(body, "LEADING INSTRUCTIONS") || !strings.Contains(body, "ORIGINAL HISTORY") ||
		!strings.Contains(body, "CONTEXT CHECKPOINT COMPACTION") ||
		strings.Contains(body, "compaction_trigger") || strings.Contains(body, `"name":"shell"`) {
		t.Fatalf("wrong upstream compaction request: %s %s", upstream.path, body)
	}
	upstream.reply = sse(`data: {"type":"response.completed","response":{"id":"resp_next","status":"completed","output":[]}}`)
	replay := httptest.NewRecorder()
	server.ServeHTTP(replay, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"fake/m1","stream":true,"input":[{"type":"compaction","encrypted_content":"`+result.Output[0].Encrypted+`"},
		{"type":"message","role":"user","content":"Continue"}]}`)))
	if replay.Code != 200 || upstream.calls != 2 || !strings.Contains(string(upstream.got), "CHECKPOINT SUMMARY") ||
		strings.Contains(string(upstream.got), result.Output[0].Encrypted) {
		t.Fatalf("checkpoint could not be replayed: %d %s upstream=%s", replay.Code, replay.Body.String(), upstream.got)
	}
}

func TestStandaloneResponsesCompactRespectsCodexTier(t *testing.T) {
	upstream := &fake{t: t, reply: deepseekSummary(provider.Responses)}
	setup(t, provider.Responses, upstream)
	if err := provider.SetCodexServiceTier(settings.CodexTierStandard); err != nil {
		t.Fatal(err)
	}
	server := New()
	t.Cleanup(server.idle.stop)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses/compact",
		strings.NewReader(`{"model":"fake/m1","input":"History","service_tier":"priority"}`))
	request.Header.Set("User-Agent", "codex_cli_rs/0.170.0")
	request.Header.Set("originator", "codex_cli_rs")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || upstream.calls != 1 || len(memberOrNil(upstream.got, "service_tier")) != 0 {
		t.Fatalf("Codex tier was not applied: status=%d calls=%d upstream=%s", response.Code, upstream.calls, upstream.got)
	}
}

func TestStandaloneResponsesCompactRefusesInputWithoutFullHistory(t *testing.T) {
	upstream := &fake{t: t}
	setup(t, provider.Responses, upstream)
	for _, input := range []string{
		`"previous_response_id":"resp_incomplete","input":"New message"`,
		`"conversation":{"id":"conv_incomplete"},"input":"New message"`,
		`"input":null`,
		`"input":[]`,
		`"input":[null,{"role":"user","content":"text"}]`,
		`"input":"  "`,
		`"input":[{"type":"item_reference","id":"msg_incomplete"}]`,
		`"input":[{"type":"configuration_update","reasoning":{"effort":"high"}}]`,
		`"input":[{"type":"configuration_\u0075pdate"}]`,
		`"input":[{"type":"compaction_trigger"}]`,
	} {
		code, response := post(t, "/v1/responses/compact", `{"model":"fake/m1",`+input+`}`)
		if code != 400 || upstream.calls != 0 {
			t.Fatalf("incomplete or unsupported history reached upstream: input=%s status=%d calls=%d response=%s", input, code, upstream.calls, response)
		}
	}
}

func TestStandaloneResponsesCompactRejectsDamagedCheckpoint(t *testing.T) {
	upstream := &fake{t: t, reply: deepseekSummary(provider.Responses)}
	setup(t, provider.Responses, upstream)
	for _, path := range []string{"/v1/responses/compact", "/responses/compact"} {
		for _, kind := range []string{"compaction", "compaction_summary"} {
			for _, checkpoint := range []string{"magpie1:invalid!", "magpie1:", "magpie1:/w=="} {
				code, reply := post(t, path, `{"model":"fake/m1","input":[{"type":"`+kind+
					`","encrypted_content":"`+checkpoint+`"},{"role":"user","content":"Later history"}]}`)
				if code != http.StatusBadRequest || upstream.calls != 0 {
					t.Fatalf("%s %s %q: status=%d calls=%d response=%s", path, kind, checkpoint, code, upstream.calls, reply)
				}
			}
		}
	}
}

func TestStandaloneResponsesCompactKeepsOpaqueCheckpointOutOfSummary(t *testing.T) {
	upstream := &fake{t: t, reply: deepseekSummary(provider.Responses)}
	setup(t, provider.Responses, upstream)
	for _, path := range []string{"/v1/responses/compact", "/responses/compact"} {
		for _, kind := range []string{"compaction", "compaction_summary"} {
			for _, value := range []string{`"vendor-sealed-checkpoint"`, "null", "17", `""`} {
				code, reply := post(t, path, `{"model":"fake/m1","input":[{"type":"`+kind+
					`","encrypted_content":`+value+`},{"role":"user","content":"Later history"}]}`)
				if code != http.StatusBadRequest || upstream.calls != 0 {
					t.Fatalf("%s %s %s: status=%d calls=%d response=%s", path, kind, value, code, upstream.calls, reply)
				}
			}
		}
	}
}

func TestStandaloneResponsesCompactDoesNotReplayDirectToolOnlyResult(t *testing.T) {
	upstream := &fake{t: t, reply: deepseekCalls(provider.Responses)}
	setup(t, provider.Responses, upstream)
	server := New()
	t.Cleanup(server.idle.stop)
	reply := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/responses/compact",
		`{"model":"fake/m1","input":"History"}`)
	if reply.Code != http.StatusBadGateway || upstream.calls != 1 || reply.Header().Get("X-Should-Retry") != "false" ||
		!strings.Contains(reply.Body.String(), "no summary") {
		t.Fatalf("completed compaction replayed: status=%d calls=%d headers=%v response=%s", reply.Code, upstream.calls, reply.Header(), reply.Body.String())
	}
	if rows := usage.Load(time.Time{}); len(rows) != 1 {
		t.Fatalf("expected one paid attempt in the ledger: %+v", rows)
	}
}

func TestStandaloneResponsesCompactUsesGatewayRestrictions(t *testing.T) {
	upstream := &fake{t: t}
	setup(t, provider.Responses, upstream)
	keys, secrets := newCaller(t, "Limited")
	if _, err := access.Update("models-key", access.Change{Key: keys[0].ID, Models: []string{"another/*"}}); err != nil {
		t.Fatal(err)
	}
	server := New()
	t.Cleanup(server.idle.stop)
	server.requestLimits.body = 256
	handler := server.Front()
	for _, scenario := range []struct {
		path, input string
		status      int
	}{
		{"/v1/responses/compact", "short input", http.StatusForbidden},
		{"/responses/compact", "short input", http.StatusForbidden},
		{"/v1/responses/compact", strings.Repeat("a", 256), http.StatusRequestEntityTooLarge},
	} {
		request := httptest.NewRequest(http.MethodPost, scenario.path,
			strings.NewReader(`{"model":"fake/m1","input":"`+scenario.input+`"}`))
		request.Header.Set("Authorization", "Bearer "+secrets[0])
		request.RemoteAddr = "127.0.0.1:12345"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != scenario.status || !json.Valid(response.Body.Bytes()) || upstream.calls != 0 {
			t.Fatalf("%s: status=%d calls=%d body=%s", scenario.path, response.Code, upstream.calls, response.Body.String())
		}
	}
}

func TestStandaloneResponsesCompactKeepsTerminalPolicy(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		name, status := "request refusal", http.StatusBadRequest
		if accepted {
			name, status = "accepted", http.StatusConflict
		}
		t.Run(name, func(t *testing.T) {
			var sends atomic.Int32
			id := installPolicyRouteFixture(t, func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				refusal := policyFixtureRefusal("request-refusal", "request", nil, http.StatusBadRequest)
				refusal["accepted"] = accepted
				json.NewEncoder(w).Encode(refusal)
			})
			server := New()
			t.Cleanup(server.idle.stop)
			reply := mediaFixtureCall(server.Front(), http.MethodPost, "/v1/responses/compact",
				`{"model":"`+id+`/free","input":[{"role":"developer","content":"Instructions"},{"role":"user","content":"History"}]}`)
			if reply.Code != status || reply.Header().Get("X-Should-Retry") != "false" || !json.Valid(reply.Body.Bytes()) || sends.Load() != 1 {
				t.Fatalf("terminal policy lost: status=%d sends=%d headers=%v body=%s", reply.Code, sends.Load(), reply.Header(), reply.Body.String())
			}
			rows := usage.Load(time.Time{})
			if len(rows) != 1 || rows[0].Status != status {
				t.Fatalf("terminal outcome missing from ledger: %+v", rows)
			}
		})
	}
}

func TestResponsesCompactionDoesNotReplayCompletedToolOnlyResult(t *testing.T) {
	for _, path := range []string{"/v1/responses/compact", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			var sends atomic.Int32
			id := installPolicyRouteFixture(t, func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				json.NewEncoder(w).Encode(map[string]string{"chatStream": deepseekCalls(provider.Chat)})
			})
			server := New()
			t.Cleanup(server.idle.stop)
			body := `{"model":"` + id + `/free","input":"History"}`
			if path == "/v1/responses" {
				body = strings.ReplaceAll(compactExecRequest, "MODEL", id+"/free")
			}
			reply := mediaFixtureCall(server.Front(), http.MethodPost, path, body)
			if reply.Code != http.StatusConflict || reply.Header().Get("X-Should-Retry") != "false" ||
				reply.Header().Get("X-Magpie-Operation") == "" || !json.Valid(reply.Body.Bytes()) || sends.Load() != 1 {
				t.Fatalf("completed operation replayed: status=%d sends=%d headers=%v body=%s", reply.Code, sends.Load(), reply.Header(), reply.Body.String())
			}
			if rows := usage.Load(time.Time{}); len(rows) != 1 {
				t.Fatalf("expected one paid attempt in the ledger: %+v", rows)
			}
		})
	}
}

// #1301: Codex compacts remotely (compaction v2: a compaction_trigger on
// /responses, exactly one compaction item back) for any provider table
// named "OpenAI", as CC Switch names its relay tables. magpie points those
// tables at /v1 while a magpie model is on, so the trigger reached /v1, went
// on to the relay as it came, and the relay's message came back: "remote
// compaction v2 expected exactly one compaction output item, got 0 from 1
// output items". On /v1 the summary is magpie's to make, as on Codex's
// backend path, and magpie's summary goes back to the model as text.
func TestCodexCompactionTriggerOnV1(t *testing.T) {
	f := &fake{t: t, reply: sse(
		`data: {"type":"response.created","response":{"id":"resp_r1","status":"in_progress","output":[]}}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"SUMMARY"}]}}`,
		`data: {"type":"response.completed","response":{"id":"resp_r1","status":"completed","output":[]}}`)}
	setup(t, provider.Responses, f)
	code, body := post(t, "/v1/responses", `{"model":"fake/m1","instructions":"You are Codex","stream":true,"store":false,
	  "include":["reasoning.encrypted_content"],"prompt_cache_key":"thread-1","tool_choice":"auto","parallel_tool_calls":true,
	  "tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],
	  "input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]},{"type":"compaction_trigger"}]}`)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if strings.Contains(string(f.got), "compaction_trigger") || !strings.Contains(string(f.got), "CONTEXT CHECKPOINT COMPACTION") {
		t.Errorf("relay got %s", f.got)
	}
	// what Codex's collect_compaction_output counts: output_item.done items
	var done []map[string]any
	for _, e := range events(body) {
		if e["type"] == "response.output_item.done" {
			done = append(done, e["item"].(map[string]any))
		}
	}
	if len(done) != 1 || done[0]["type"] != "compaction" {
		t.Fatalf("Codex wants exactly one compaction output item, got %v\n%s", done, body)
	}
	enc, _ := done[0]["encrypted_content"].(string)
	if b, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(enc, magpieCompaction)); string(b) != "SUMMARY" {
		t.Errorf("compaction holds %q", enc)
	}

	// the next turn carries the compaction item; the relay gets its text
	f.reply = sse(`data: {"type":"response.completed","response":{"id":"resp_r2","status":"completed","output":[]}}`)
	code, body = post(t, "/v1/responses", `{"model":"fake/m1","stream":true,
	  "input":[{"type":"compaction","encrypted_content":"`+enc+`"},{"type":"message","role":"user","content":[{"type":"input_text","text":"go on"}]}]}`)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if strings.Contains(string(f.got), magpieCompaction) || !strings.Contains(string(f.got), "SUMMARY") {
		t.Errorf("relay got %s", f.got)
	}
}
