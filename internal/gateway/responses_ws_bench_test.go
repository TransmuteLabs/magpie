package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func BenchmarkResponsesSocketReadTurnState(b *testing.B) {
	for _, fixture := range []struct {
		name     string
		items    int
		metadata string
	}{
		{name: "128-items-no-state", items: 128},
		{name: "4-items-with-state", items: 4, metadata: `"client_metadata":{"turn_id":"turn-2","x-codex-turn-state":"fixture-state"},`},
		{name: "128-items-with-state", items: 128, metadata: `"client_metadata":{"turn_id":"turn-2","x-codex-turn-state":"fixture-state"},`},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			body := []byte(`{` + fixture.metadata + `"input":[` +
				strings.Join(codexSocketHistory(fixture.items), ",") + `],"model":"gpt-5.5","stream":true}`)
			origin := httptest.NewRequest("GET", "/v1/responses", nil)
			socket := &responsesSocket{o: &socketOrigin{req: origin}}
			recorder := httptest.NewRecorder()
			b.ReportAllocs()
			for b.Loop() {
				req, _ := socket.request(b.Context(), body, fixture.metadata != "")
				buffer, status, err := readBoundedRequestBody(recorder, req, requestLimits{})
				if err != nil || status != 0 || len(buffer) != int(req.ContentLength) {
					b.Fatalf("bounded read returned %d bytes, status %d, error %v", len(buffer), status, err)
				}
			}
		})
	}
}
