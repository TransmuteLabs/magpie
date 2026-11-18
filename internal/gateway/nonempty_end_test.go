package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestIndependentMergeNonemptyResponsesCompletionStaysTheAnswer(t *testing.T) {
	fresh(t)
	completed := sse(
		`event: response.created`+"\n"+`data: {"type":"response.created","response":{"id":"r_complete","status":"in_progress","output":[]}}`,
		`event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"id":"r_complete","model":"m","object":"response","status":"completed","output":[{"id":"msg_complete","type":"message","role":"assistant","content":[{"type":"output_text","text":"completed with the real answer","annotations":[]}]}],"usage":{"input_tokens":17,"output_tokens":6,"total_tokens":23}}}`,
	)
	first := &scripted{replies: []reply{{200, "text/event-stream", completed}}}
	second := &scripted{replies: []reply{{200, "text/event-stream", chatFromB}}}
	scriptedOn(t, "a", provider.Responses, first)
	scriptedOn(t, "b", provider.Chat, second)
	if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{"a/m", "b/m"}, Routing: provider.Ordered}); err != nil {
		t.Fatal(err)
	}
	server := New()
	t.Cleanup(server.idle.stop)
	record := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"group/g","stream":true,"input":"hi"}`))
	request.RemoteAddr = "127.0.0.1:12345"
	server.Front().ServeHTTP(record, request)
	status, body := record.Code, record.Body.String()
	if status != 200 || second.n != 0 || !strings.Contains(body, "completed with the real answer") {
		t.Fatalf("a nonempty completed response was discarded as empty: status=%d second=%d body=%s", status, second.n, body)
	}
}

func TestIndependentMergeLegacyFunctionCallStaysTheAnswer(t *testing.T) {
	fresh(t)
	called := `{"id":"chat_call","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null,"function_call":{"name":"lookup","arguments":"{}"}},"finish_reason":"function_call"}],"usage":{"prompt_tokens":17,"completion_tokens":6,"total_tokens":23}}`
	first := &scripted{replies: []reply{{200, "application/json", called}}}
	second := &scripted{replies: []reply{{200, "application/json", chatOK}}}
	third := &scripted{replies: []reply{{200, "application/json", chatOK}}}
	threeGroup(t, first, second, third)
	server := New()
	t.Cleanup(server.idle.stop)
	status, body := postAs(t, server, "", `{"model":"group/g","messages":[{"role":"user","content":"lookup"}],"functions":[{"name":"lookup","parameters":{"type":"object","properties":{}}}]}`)
	if status != 200 || second.n != 0 || !strings.Contains(body, `"function_call"`) {
		t.Fatalf("a nonempty legacy function call was discarded as empty: status=%d second=%d body=%s", status, second.n, body)
	}
}

func TestIndependentMergeNonemptySecondChoiceStaysTheAnswer(t *testing.T) {
	fresh(t)
	answered := sse(
		`data: {"id":"multi","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"},{"index":1,"delta":{"role":"assistant","content":"the second choice answered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":6}}`,
		`data: [DONE]`,
	)
	first := &scripted{replies: []reply{{200, "text/event-stream", answered}}}
	second := &scripted{replies: []reply{{200, "text/event-stream", chatFromB}}}
	third := &scripted{replies: []reply{{200, "text/event-stream", chatFromB}}}
	threeGroup(t, first, second, third)
	server := New()
	t.Cleanup(server.idle.stop)
	status, body := postAs(t, server, "", `{"model":"group/g","stream":true,"n":2,"messages":[{"role":"user","content":"hi"}]}`)
	if status != 200 || second.n != 0 || !strings.Contains(body, "the second choice answered") {
		t.Fatalf("a nonempty second choice was discarded as empty: status=%d second=%d body=%s", status, second.n, body)
	}
}

func TestSplitChoicesDoNotEndAtTheFirstEmptyChoice(t *testing.T) {
	fresh(t)
	answered := sse(
		`data: {"id":"multi","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"multi","object":"chat.completion.chunk","choices":[{"index":1,"delta":{"role":"assistant","content":"the later choice answered"}}]}`,
		`data: {"id":"multi","object":"chat.completion.chunk","choices":[{"index":1,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":6}}`,
		`data: [DONE]`,
	)
	first := &scripted{replies: []reply{{200, "text/event-stream", answered}}}
	second := &scripted{replies: []reply{{200, "text/event-stream", chatFromB}}}
	third := &scripted{replies: []reply{{200, "text/event-stream", chatFromB}}}
	threeGroup(t, first, second, third)
	server := New()
	t.Cleanup(server.idle.stop)
	record := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"group/g","stream":true,"n":2,"messages":[{"role":"user","content":"hi"}]}`))
	request.RemoteAddr = "127.0.0.1:12345"
	server.Front().ServeHTTP(record, request)
	if record.Code != 200 || second.n != 0 || third.n != 0 || !strings.Contains(record.Body.String(), "the later choice answered") {
		t.Fatalf("a later choice was discarded: status=%d second=%d third=%d body=%s", record.Code, second.n, third.n, record.Body)
	}
}

func TestCompletedOutputMustBeExplicitlyEmpty(t *testing.T) {
	for _, test := range []struct {
		name, output string
		kind         int
	}{
		{"missing", "", eventContent},
		{"null", `,"output":null`, eventEmptyEnd},
		{"empty", `,"output":[]`, eventEmptyEnd},
		{"text", `,"output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}]`, eventContent},
		{"call", `,"output":[{"type":"function_call","name":"lookup","arguments":"{}"}]`, eventContent},
		{"reasoning", `,"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]}]`, eventContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := `event: response.completed` + "\n" + `data: {"type":"response.completed","response":{"status":"completed"` + test.output + `,"usage":{"input_tokens":17,"output_tokens":6}}}`
			if kind, _, _ := streamEvent([]byte(event)); kind != test.kind {
				t.Fatalf("completed output classified as %d, want %d: %s", kind, test.kind, event)
			}
		})
	}
}

func TestLegacyFunctionCallIsNeitherEmptyNorARefusal(t *testing.T) {
	for _, test := range []struct {
		name, call string
		refused    bool
		empty      bool
	}{
		{"call", `{"name":"lookup","arguments":"{}"}`, false, false},
		{"null", "null", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, reason := range []string{"stop", "function_call", "content_filter"} {
				body := `{"choices":[{"message":{"content":null,"function_call":` + test.call + `},"finish_reason":"` + reason + `"}]}`
				_, refused, empty := readWhole([]byte(body))
				if refused != (test.refused && reason == "content_filter") || empty != (test.empty && reason != "content_filter") {
					t.Errorf("legacy call status lost: reason=%s refused=%v empty=%v", reason, refused, empty)
				}
			}
		})
	}
}

func TestGeminiCandidatesDoNotEndAtTheFirstEmptyCandidate(t *testing.T) {
	for _, test := range []struct {
		name   string
		events []string
	}{
		{"same event", []string{`data: {"candidates":[{"index":0,"content":{"parts":[]},"finishReason":"STOP"},{"index":1,"content":{"parts":[{"text":"the later candidate answered"}]},"finishReason":"STOP"}]}`}},
		{"split events", []string{
			`data: {"candidates":[{"index":0,"content":{"parts":[]},"finishReason":"STOP"}]}`,
			`data: {"candidates":[{"index":1,"content":{"parts":[{"text":"the later candidate answered"}]},"finishReason":"STOP"}]}`,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fresh(t)
			first := &scripted{replies: []reply{{200, "text/event-stream", sse(test.events...)}}}
			second := &scripted{replies: []reply{{200, "text/event-stream", sse(`data: {"candidates":[{"content":{"parts":[{"text":"fallback"}]},"finishReason":"STOP"}]}`)}}}
			scriptedOn(t, "a", provider.Gemini, first)
			scriptedOn(t, "b", provider.Gemini, second)
			if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{"a/m", "b/m"}, Routing: provider.Ordered}); err != nil {
				t.Fatal(err)
			}
			server := New()
			t.Cleanup(server.idle.stop)
			record := httptest.NewRecorder()
			request := httptest.NewRequest("POST", "/v1beta/models/group/g:streamGenerateContent?alt=sse", strings.NewReader(`{"generationConfig":{"candidateCount":2},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
			request.RemoteAddr = "127.0.0.1:12345"
			server.Front().ServeHTTP(record, request)
			if record.Code != 200 || second.n != 0 || !strings.Contains(record.Body.String(), "the later candidate answered") {
				t.Fatalf("a later candidate was discarded: status=%d second=%d body=%s", record.Code, second.n, record.Body)
			}
		})
	}
}

func TestWholeReplyWithAnotherAnswerIsNotARefusal(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"chat", `{"choices":[{"message":{"content":null},"finish_reason":"content_filter"},{"message":{"content":"answer"},"finish_reason":"stop"}]}`},
		{"gemini", `{"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"},{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, refused, empty := readWhole([]byte(test.body)); refused || empty {
				t.Fatalf("a whole multi-answer reply was discarded: refused=%v empty=%v body=%s", refused, empty, test.body)
			}
		})
	}
}
