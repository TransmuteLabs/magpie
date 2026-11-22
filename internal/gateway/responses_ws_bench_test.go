package gateway

import (
	"context"
	"net/http"
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

func socketTerminalFrame(outputBytes int) []byte {
	return []byte(`{"type":"response.completed","response":{"id":"resp_fixture","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` +
		strings.Repeat("x", outputBytes) + `"}]}],"usage":{"input_tokens":20,"output_tokens":4,"total_tokens":24}}}`)
}

func BenchmarkResponsesSocketTerminalRelay(b *testing.B) {
	ctx, cancel := context.WithCancel(b.Context())
	cancel()
	for _, fixture := range []struct {
		name     string
		frame    []byte
		terminal bool
	}{
		{name: "terminal_2KiB", frame: socketTerminalFrame(2 << 10), terminal: true},
		{name: "terminal_128KiB", frame: socketTerminalFrame(128 << 10), terminal: true},
		{name: "delta_2KiB", frame: []byte(`{"type":"response.output_text.delta","delta":"` + strings.Repeat("x", 2<<10) + `"}`)},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			wire := []byte("data: " + string(fixture.frame) + "\n\n")
			h := http.Header{"Content-Type": {"text/event-stream"}}
			socket := &responsesSocket{}
			b.SetBytes(int64(len(wire)))
			b.ReportAllocs()
			for b.Loop() {
				writer := socketTurnWriter{rs: socket, ctx: ctx, run: &socketTurnRun{}, h: h}
				n, err := writer.Write(wire)
				writer.finish()
				if err != nil || n != len(wire) || writer.completed != fixture.terminal {
					b.Fatalf("wrote %d/%d bytes, completed %v, error %v", n, len(wire), writer.completed, err)
				}
			}
		})
	}
}
