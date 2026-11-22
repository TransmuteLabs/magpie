package gateway

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestResponsesPromptEstimateMatchesText(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"ordinary", `{"instructions":"be brief","tools":[{"type":"web_search"}],"input":[` + userPing + `,` + asstPong + `,` + userAgain + `]}`},
		{"optional message fields", `{"input":[{"id":"m1","type":"message","status":"completed","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"ok","annotations":[],"logprobs":[]}]}]}`},
		{"inferred message", `{"input":[{"role":"user","content":[{"text":"  hi  ","type":"input_text"}]}]}`},
		{"blank and empty", `{"input":[{"role":"user","content":[{"type":"input_text","text":"   "}]},{"role":"user","content":[{"type":"input_text","text":""}]},` + userAgain + `]}`},
		{"escapes and Unicode", `{"previous_response_id":"r1","input":[{"role":"user","content":[{"type":"input_text","text":"你好 \uD83D\uDE04\t\\ \""}]},{"role":"assistant","content":[{"type":"output_text","text":"` + "\xff" + `"}]}]}`},
		{"context and notices", `{"input":[{"role":"user","content":[{"type":"input_text","text":"  <environment_context>shell</environment_context>  "}]},{"role":"user","content":[{"type":"input_text","text":"<system-reminder>check</system-reminder>"}]},{"role":"user","content":[{"type":"input_text","text":"Available agent types for the Agent tool: explorer"}]},{"role":"user","content":[{"type":"input_text","text":" \u003cturn_aborted\u003eold request"}]},{"role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /tmp/demo\n\nRead the tests."}]},{"role":"user","content":[{"type":"input_text","text":"<in-app-browser-context>view\n## My request:\nreply"}]},` + userAgain + `]}`},
		{"multipart and images", `{"input":[{"role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"https://example.invalid/p.png","detail":"low"},{"type":"input_text","text":"again"}]}]}`},
		{"system and developer", `{"input":[{"role":"system","content":[{"type":"input_text","text":"be concise"}]},{"role":"developer","content":[{"type":"output_text","text":"write in Go"}]},` + userPing + `]}`},
		{"duplicate members", `{"input":[{"type":"reasoning","type":"message","role":"developer","\u0072ole":"user","content":[{"type":"input_text","t\u0065xt":"first","text":"last"}]},{"role":"assistant","content":[{"type":"input_image","type":"output_text","text":"reply"}]}]}`},
		{"legacy field spellings", `{"input":[{"ROLE":"user","CONTENT":[{"TYPE":"input_text","TEXT":"Case"}]},{"role":"user","content":"text shorthand"}]}`},
		{"wrong field types", `{"input":[{"role":"user","content":[{"type":"input_text","text":null,"detail":1}]},{"role":"user","content":[{"type":"input_text","text":1,"text":"last"}]},{"role":"user","arguments":5,"content":[{"type":"input_text","text":"ignored"}]}]}`},
		{"malformed body", `{"input":[{"role":"user","content":[{"type":"input_text","text":"oops\k"}]}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(test.body)
			estimate := promptOf(provider.Responses, body)
			pieces, ok := promptPieces(provider.Responses, body)
			if !ok {
				if estimate != nil || test.name != "malformed body" {
					t.Fatal("estimate and text reader disagree on invalid body")
				}
				return
			}
			want := pieces.prompt()
			clear(body)
			if !reflect.DeepEqual(estimate, want) {
				t.Fatalf("estimate %+v, text reader %+v", estimate, want)
			}
			wire := &upBytes{wire: len(test.body) / 2}
			if got, want := upParts(estimate, wire), upParts(want, wire); !reflect.DeepEqual(got, want) {
				t.Fatalf("estimated up parts %+v, text reader %+v", got, want)
			}
		})
	}
}

func TestResponsesPromptTextKeepsExactConversation(t *testing.T) {
	body := []byte(`{"instructions":"be brief","tools":[{"type":"web_search"}],"input":[` +
		userPing + `,` + asstPong + `,{"role":"user","content":[{"type":"input_text","text":"  again  "}]}]}`)
	estimate := promptOf(provider.Responses, body)
	if estimate == nil || estimate.Turns != 2 || estimate.Tokens != 13 ||
		estimate.sent != (sentSizes{system: 8, tools: 21, all: 46}) {
		t.Fatalf("prompt = %+v", estimate)
	}
	detached := &Server{}
	detached.trace.keepBody(1, provider.Responses, body, false)
	card, ok := detached.PromptText(1, PartChat, true)
	if !ok || len(card.Items) != 2 {
		t.Fatalf("chat card = %+v", card)
	}
	var got []string
	for _, item := range card.Items {
		for _, piece := range item.Pieces {
			got = append(got, item.Name+":"+piece.Role+":"+piece.Text)
		}
	}
	want := []string{"1:user:ping", "1:assistant:pong", "2:user:  again  "}
	if !slices.Equal(got, want) {
		t.Fatalf("chat pieces %q, want %q", got, want)
	}
}

func BenchmarkPromptResponsesHistory(b *testing.B) {
	history := make([]json.RawMessage, 0, 128)
	for index := range 128 {
		template, marker := userPing, "ping"
		if index%2 == 0 {
			template, marker = asstPong, "pong"
		}
		prefix := fmt.Sprintf("history %03d ", index)
		text := prefix + strings.Repeat("x", 2048-len(prefix))
		history = append(history, json.RawMessage(strings.Replace(template, marker, text, 1)))
	}
	reconstructed := slices.Clone(history)
	for index := range 24 {
		reply := fmt.Sprintf(`{"type":"message","id":"msg_%02d","status":"completed","role":"assistant","content":[{"type":"output_text","text":"fixture answer %02d"}]}`, index, index)
		next := strings.Replace(userAgain, "again", fmt.Sprintf("new message %02d", index+1), 1)
		reconstructed = append(reconstructed, json.RawMessage(reply), json.RawMessage(next))
	}
	edited := slices.Clone(reconstructed)
	edited[0] = json.RawMessage(strings.Replace(string(edited[0]), "history", "History", 1))
	edited = append(edited, json.RawMessage(asstPong), json.RawMessage(userAgain))
	mixed := slices.Clone(reconstructed)
	for index := 1; index < len(history); index += 2 {
		raw := strings.Replace(string(mixed[index]), strings.Repeat("x", 16), `Read \"\u4f60\u597d\". `, 64)
		mixed[index] = json.RawMessage(raw)
	}

	for _, scenario := range []struct {
		name  string
		body  []json.RawMessage
		turns int
	}{
		{"first", history, 65},
		{"delta-reconstructed", reconstructed, 89},
		{"edited-full", edited, 90},
		{"mixed-fallback", mixed, 89},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			body, err := turnBody(map[string]json.RawMessage{
				"model":            json.RawMessage(`"gpt-5.5"`),
				"instructions":     json.RawMessage(`"be brief"`),
				"prompt_cache_key": json.RawMessage(`"history"`),
				"stream":           json.RawMessage(`true`),
				"store":            json.RawMessage(`false`),
			}, scenario.body)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				if estimate := promptOf(provider.Responses, body); estimate == nil || estimate.Turns != scenario.turns {
					b.Fatalf("prompt = %+v, want %d turns", estimate, scenario.turns)
				}
			}
		})
	}
}
