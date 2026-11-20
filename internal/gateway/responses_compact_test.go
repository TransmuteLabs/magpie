package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
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
	f := &fake{t: t, reply: deepseekSummary(provider.Responses)}
	setup(t, provider.Responses, f)
	if err := provider.SetNativeCompaction("fake", true); err != nil {
		t.Fatal(err)
	}
	code, body := post(t, "/v1/responses/compact", `{"model":"fake/m1","input":[{"type":"compaction","encrypted_content":"magpie1:UHJldmlvdXMgc3VtbWFyeQ=="},{"role":"user","content":"new history"}]}`)
	if code != 200 || !strings.Contains(body, "magpie1:") ||
		f.calls != 1 || f.path != "/v1/responses" || !strings.Contains(string(f.got), "Previous summary") {
		t.Fatalf("checkpoint was not decoded locally: status=%d calls=%d path=%q upstream=%s response=%s", code, f.calls, f.path, f.got, body)
	}
}
