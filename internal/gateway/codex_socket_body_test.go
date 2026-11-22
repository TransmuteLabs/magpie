package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestCodexSocketMessageUsesTheSelectedReplayBody(t *testing.T) {
	outgoing := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	provider.SetBody(outgoing, []byte(`{"stream":true,"input":"old"}`))
	replay := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	expected := []byte(`{"stream":true,"input":"` + strings.Repeat("x", 256<<10) + `"}`)
	provider.SetBody(replay, expected)
	calls := 0
	outgoing.GetBody = func() (io.ReadCloser, error) {
		calls++
		return replay.GetBody()
	}
	message, ok := codexSocketMessage(outgoing)
	if !ok || !bytes.Equal(message, expected) || calls != 1 {
		t.Fatalf("replay selection failed: got %d bytes (ok=%v), %d GetBody calls", len(message), ok, calls)
	}
	if &message[0] != &expected[0] {
		t.Fatalf("copied the %d-byte replay body", len(message))
	}
	original, err := io.ReadAll(outgoing.Body)
	if err != nil || string(original) != `{"stream":true,"input":"old"}` {
		t.Fatalf("the original reader changed: %s, %v", original, err)
	}
	provider.LetGo(replay)
	if message, ok := codexSocketMessage(outgoing); ok || message != nil {
		t.Fatal("a released replay body could still be read")
	}
}

func TestCodexSocketMessageReplayFormats(t *testing.T) {
	expected := []byte(`{"stream":true,"input":"kept"}`)
	for _, variant := range []string{"wrapped", "zstd", "unknown encoding", "GetBody error", "invalid JSON"} {
		t.Run(variant, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			provider.SetBody(req, expected)
			want := true
			switch variant {
			case "wrapped":
				replay := req.GetBody
				req.GetBody = func() (io.ReadCloser, error) {
					reader, err := replay()
					return struct{ io.ReadCloser }{reader}, err
				}
			case "zstd":
				provider.SetCodexZstdBody(req, zstdEncode(expected))
			case "unknown encoding":
				req.Header.Set("Content-Encoding", "other")
				want = false
			case "GetBody error":
				req.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("unavailable") }
				want = false
			case "invalid JSON":
				provider.SetBody(req, []byte(`{"stream":true,"input":[`))
				want = false
			}
			message, ok := codexSocketMessage(req)
			if ok != want || want && !bytes.Equal(message, expected) || !want && message != nil {
				t.Fatalf("message %q, ok=%v, want ok=%v", message, ok, want)
			}
			if want && &message[0] == &expected[0] {
				t.Fatal("an opaque or encoded reader returned borrowed input")
			}
		})
	}
}

func BenchmarkCodexSocketReplayBody(bench *testing.B) {
	body := []byte(`{"stream":true,"input":"` + strings.Repeat("x", 256<<10) + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	provider.SetBody(req, body)
	bench.ReportAllocs()
	bench.SetBytes(int64(len(body)))
	for bench.Loop() {
		message, ok := codexSocketMessage(req)
		if !ok || len(message) != len(body) {
			bench.Fatal("failed to read the replay body")
		}
	}
}
