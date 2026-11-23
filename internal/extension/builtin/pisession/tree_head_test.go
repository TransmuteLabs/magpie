package pisession

import (
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func inspectWithJSON(line []byte, spokenOnly bool) (sdk.SessionTreeNode, bool) {
	var head struct {
		treeLine[messageHead]
		Summary summaryHead `json:"summary"`
	}
	if json.Unmarshal(line, &head) != nil || head.ID == "" || head.Type == "session" || head.Type == "title" {
		return sdk.SessionTreeNode{}, false
	}
	hasContent := head.Type == "message" || head.Type == "compaction" || head.Type == "branch_summary"
	if spokenOnly {
		hasContent = head.Type == "message" && head.Message != nil && (head.Message.Role == "user" || head.Message.Role == "assistant")
	}
	return sdk.SessionTreeNode{ID: head.ID, ParentID: head.ParentID, HasContent: hasContent}, true
}

func TestTreeHeadMatchesJSON(t *testing.T) {
	cases := []string{
		`{"type":"message","id":"hello","parentId":"world","message":{"role":"assistant","content":[{"type":"text","text":"x \" y \\\\"}],"model":"m"}}`,
		`{"type":"message","id":"hello","parentId":null,"message":{"role":"toolResult","toolName":"bash","content":"}{ [ \" \\ q"}}`,
		`{"type":"message","id":"世界","parentId":"мир","message":{"role":"user","content":null}}`,
		`{"id":"a","type":"branch_summary","summary":"\uD800","message":null}`,
		`{"id":"a","type":"compaction","summary":null,"message":{"role":null,"toolName":null,"model":null}}`,
		`{"id":"a","type":"custom","unknown":{"type":"message","id":"shadow","summary":17},"message":{"id":"shadow","content":[0,0.1,-1.2e+5,true,false,{},[]]}}`,
		"{\t\"type\" : \"message\" , \"id\" : \"a\" , \"parentId\" : \"b\" , \"message\" : { \"role\" : \"user\" } }\r\n",
		` {"type":"message","id":"a","message":{"role":"user"}} `,
		`{"id":"a","id":null,"parentId":"b","parentId":null,"type":"message","type":null,"message":{"role":"user","role":null}}`,
		`{"id":"a","id":"b","parentId":"c","parentId":"d","type":"title","type":"message","message":{"role":"toolResult","role":"user"}}`,
		`{"i\u0064":"escaped","ID":"folded","PARENTID":"p","tYpe":"message","message":{"r\u006fle":"assistant","ROLE":null}}`,
		`{"kind":"extension","id":"a","type":"m\u0065ssage","message":{"role":"ass\u0069stant"}}`,
		`{"id":"\uD800","parentId":"\uD834\uDD1E","type":"message","message":{"role":"user"}}`,
		"{\"id\":\"\xff\",\"parentId\":\"\xc0\",\"type\":\"message\",\"message\":{\"role\":\"user\"}}",
		`{"id":"a","type":"message","message":{"role":"user","model":"m"},"message":{"toolName":"search"}}`,
		`{"id":"a","type":"message","message":{"role":"user"},"message":null,"message":{"model":"m"}}`,
		`{"id":"a","type":"message","message":{"role":"assistant"},"message":{"role":null}}`,
		`{"id":"a","type":"message","message":{"role":"assistant"},"MESSAGE":null}`,
		`{"id":"a","type":"message","message":{"role":"assistant","toolName":4},"message":{"toolName":"valid"}}`,
		`{"id":"a","type":"message","message":{"role":"assistant","MODEL":[]}}`,
		`{"id":"a","type":"message","message":{"role":"assistant","content":[{"model":99,"role":{}}]}}`,
		`{"id":"a","type":"title","message":{"role":"user"}}`,
		`{"id":"a","type":"session","message":{"role":"user"}}`,
		`{"id":"a","type":"compaction","summary":false,"summary":"valid"}`,
		`{"id":"a","type":"compaction","ſummary":4}`,
		`{"id":"a","type":"compaction","summary":[1,2]}`,
		`{"id":"a","type":"message","parentId":true,"parentId":"p"}`,
		`{"id":"a","type":"message","message":{"role":null,"role":[],"role":"user"}}`,
		`{"id":"a","id":8,"id":"b"}`,
		`{"id":"a","type":0,"type":"message"}`,
		`{"id":"a","message":false}`,
		`{"id":"","type":"message"}`,
		`{"id":"a","unknown":{"id":"nested","type":"message","message":100}}`,
		`{"id":"a","message":{"role":"user","content":"broken\q"}}`,
		`{"id":"a","unknown":"escaped\u0000\uD800\uFFFF"}`,
		`{"id":"a","unknown":"broken\u0g00"}`,
		`{"id":"a","unknown":"broken\u00"}`,
		`{"id":"a","unknown":[1e-2,0.5,-0,[true,null,{}]]}`,
		`{"id":"a","unknown":01}`,
		`{"id":"a","unknown":1e+}`,
		`{"id":"a","unknown":1.1.1}`,
		`{"id":"a","unknown":truefalse}`,
		`{"id":"a","unknown":[1,]}`,
		`{"id":"a","unknown":{"k":1,}}`,
		`{"id":"a","unknown":{"k" 1}}`,
		`{"id":"a","unknown":[0 1]}`,
		`{"id":"a","deep":` + strings.Repeat("[", 64) + `0` + strings.Repeat("]", 64) + `}`,
		`{"id":"a","message":{"role":"user","content":[0,]}}`,
		`{"id":"a","unknown":{"id":"nested"},"more":[true]`,
		`{"id":"a"}{"id":"b"}`,
		`{"id":"a"}` + "\x00",
		`{"id":"a","deep":` + strings.Repeat("[", 10001) + `0` + strings.Repeat("]", 10001) + `}`,
		`[{"id":"a"}]`,
		`null`,
		`{}`,
		``,
	}
	for padding := range 16 {
		start := `{"id":"a","message":{"role":"user","content":"` + strings.Repeat("x", padding) + strings.Repeat(`\\\"`, 13)
		end := `" },"parentId":"b"}`
		cases = append(cases, start+end, start+"\x1f"+end, start+`\u00x0`+end, start+`\`+end)
	}
	codec := TreeCodec()
	for index, input := range cases {
		for _, spoken := range []bool{false, true} {
			line := []byte(input)
			want, expected := inspectWithJSON(line, spoken)
			if got, ok := codec.Inspect(line, spoken); got != want || ok != expected {
				t.Errorf("case %d spoken=%v got (%+v,%v), want (%+v,%v)", index, spoken, got, ok, want, expected)
			}
		}
	}
}

func TestTreeHeadOwnsIdentity(t *testing.T) {
	line := []byte(`{"type":"message","id":"complete","parentId":"previous","message":{"role":"user","content":"input"}}`)
	node, ok := TreeCodec().Inspect(line, true)
	clear(line)
	if !ok || node != (sdk.SessionTreeNode{ID: "complete", ParentID: "previous", HasContent: true}) {
		t.Fatalf("inspection borrowed mutable input: %+v, %v", node, ok)
	}
}

func BenchmarkPiInspectProjection(b *testing.B) {
	for _, fixture := range []struct{ name, payload string }{
		{"short", `ready to test`},
		{"large", strings.Repeat("0123456789 abcdef ", 4096)},
		{"escaped", strings.Repeat(`{\"a\":\"test\"}\n`, 4096)},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			line := []byte(`{"type":"message","id":"current","parentId":"before","timestamp":"2026-11-23T09:00:00Z","message":{"role":"user","content":[{"type":"text","text":"` + fixture.payload + `"}],"model":"fixture"}}`)
			inspect := TreeCodec().Inspect
			if node, ok := inspect(line, false); !ok || node.ID != "current" || node.ParentID != "before" || !node.HasContent {
				b.Fatalf("invalid benchmark input: %+v, %v", node, ok)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(line)))
			for b.Loop() {
				inspect(line, false)
			}
		})
	}
}
