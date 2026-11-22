package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesSocketMetadataPresence(t *testing.T) {
	for _, test := range []struct {
		name, fields string
	}{
		{"absent", ""},
		{"literal dotted names", `,"client_metadata.turn_id":"wrong","client_metadata.x-codex-turn-state":"wrong"`},
		{"case-sensitive name", `,"Client_Metadata":{"turn_id":"wrong","x-codex-turn-state":"wrong"}`},
		{"empty object", `,"client_metadata":{}`},
		{"null", `,"client_metadata":null`},
		{"scalar", `,"client_metadata":"a string"`},
		{"array", `,"client_metadata":[{"turn_id":"wrong","x-codex-turn-state":"wrong"}]`},
		{"string values", `,"client_metadata":{"turn_id":"turn-1","x-codex-turn-state":"state-1","ws_request_header_x_openai_internal_codex_responses_lite":"TRUE"}`},
		{"escaped names", `,"client_metadat\u0061":{"turn_\u0069d":"x","x-codex-turn-\u0073tate":"escaped\u0026value"}`},
		{"number and raw object", `,"client_metadata":{"turn_id":123.50,"x-codex-turn-state": { "escaped":"<tag>&", "valid": true }}`},
		{"array and boolean values", `,"client_metadata":{"turn_id":["x","y"],"x-codex-turn-state":false,"ws_request_header_x_openai_internal_codex_responses_lite":true}`},
		{"duplicate child values", `,"client_metadata":{"turn_id":"a","turn_id":"b","x-codex-turn-state":"first","x-codex-turn-state":"last","ws_request_header_x_openai_internal_codex_responses_lite":false,"ws_request_header_x_openai_internal_codex_responses_lite":true}`},
		{"duplicate outer name", `,"client_metadata":{"turn_id":"wrong","x-codex-turn-state":"wrong"},"client_metadat\u0061":null`},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame := []byte(`{"model":"gpt-5.5","input":[` + userPing + `]` + test.fields + `}`)
			var fields map[string]json.RawMessage
			if err := decodeJSONView(frame, &fields); err != nil {
				t.Fatal(err)
			}
			_, input := prevInput(fields["input"])
			body, err := turnBody(fields, input)
			if err != nil {
				t.Fatal(err)
			}

			const liteHeader = "X-OpenAI-Internal-Codex-Responses-Lite"
			const stateHeader = "X-Codex-Turn-State"
			origin := httptest.NewRequest("GET", "/v1/responses", nil)
			origin.Header.Set(liteHeader, "inherited-lite")
			origin.Header.Set(stateHeader, "inherited-state")
			rs := &responsesSocket{o: &socketOrigin{req: origin}}
			wantBody, wantState := body, "inherited-state"
			wantTurnID := gjson.GetBytes(body, "client_metadata.turn_id").String()
			if value := gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String(); value != "" {
				wantState = value
				wantBody = withoutClientMetadata(body, "x-codex-turn-state")
			}
			wantLite := "inherited-lite"
			if strings.EqualFold(gjson.GetBytes(wantBody, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").String(), "true") {
				wantLite = "true"
			}

			_, hasClientMetadata := fields["client_metadata"]
			req, turnID := rs.request(t.Context(), body, hasClientMetadata)
			if turnID != wantTurnID || req.Header.Get(stateHeader) != wantState || req.Header.Get(liteHeader) != wantLite {
				t.Fatalf("turn/state/lite = %q/%q/%q, want %q/%q/%q", turnID, req.Header.Get(stateHeader),
					req.Header.Get(liteHeader), wantTurnID, wantState, wantLite)
			}
			if req.ContentLength != int64(len(wantBody)) || req.Header.Get("Content-Length") != strconv.Itoa(len(wantBody)) {
				t.Fatalf("content length = %d/%q, want %d", req.ContentLength, req.Header.Get("Content-Length"), len(wantBody))
			}
			gotBody, err := io.ReadAll(req.Body)
			if err != nil || !bytes.Equal(gotBody, wantBody) {
				t.Fatalf("body was %s, %v; want %s", gotBody, err, wantBody)
			}
			reopened, err := req.GetBody()
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if gotBody, err := io.ReadAll(reopened); err != nil || !bytes.Equal(gotBody, wantBody) {
				t.Fatalf("GetBody was %s, %v; want %s", gotBody, err, wantBody)
			}
		})
	}
}

func BenchmarkResponsesSocketMetadataPresence(b *testing.B) {
	for _, fixture := range []struct {
		name  string
		items int
		meta  json.RawMessage
	}{
		{"small-absent", 4, nil},
		{"M03-absent", 128, nil},
		{"large-absent", 1024, nil},
		{"M03-turn-id", 128, json.RawMessage(`{"turn_id":"turn-1"}`)},
		{"M03-state", 128, json.RawMessage(`{"turn_id":"turn-1","x-codex-turn-state":"state-1"}`)},
	} {
		fields := map[string]json.RawMessage{"model": json.RawMessage(`"gpt-5.5"`), "stream": json.RawMessage(`true`)}
		if fixture.meta != nil {
			fields["client_metadata"] = fixture.meta
		}
		history := codexSocketHistory(fixture.items)
		input := make([]json.RawMessage, 0, len(history))
		for _, item := range history {
			input = append(input, json.RawMessage(item))
		}
		body, err := turnBody(fields, input)
		if err != nil {
			b.Fatal(err)
		}
		origin := httptest.NewRequest("GET", "/v1/responses", nil)
		rs := &responsesSocket{o: &socketOrigin{req: origin}}
		ctx := b.Context()
		for _, mode := range []struct {
			name   string
			legacy bool
		}{
			{"legacy-scans", true},
			{"check-key", false},
		} {
			b.Run(fmt.Sprintf("%s/%s", fixture.name, mode.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					hasClientMetadata := true
					if !mode.legacy {
						_, hasClientMetadata = fields["client_metadata"]
					}
					req, _ := rs.request(ctx, body, hasClientMetadata)
					if req.ContentLength < 1 {
						b.Fatal("empty request")
					}
				}
			})
		}
	}
}
