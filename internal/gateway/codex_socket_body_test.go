package gateway

import (
	"bytes"
	"context"
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

func TestCodexSocketMessageHeldValidation(t *testing.T) {
	body := []byte(`{"stream":true,"model":"base","input":[null,false,-1.25e+2,"\u263a","` + strings.Repeat("x", validMemoMin) + `"]}`)
	rewritten := rewriteModel(body, "wire")
	wantRewrite := bytes.Replace(body, []byte(`"base"`), []byte(`"wire"`), 1)
	if len(rewritten) != len(body) || &rewritten[0] == &body[0] || !bytes.Equal(rewritten, wantRewrite) {
		t.Fatal("expected model rewriting to produce fresh, same-length bytes")
	}
	invalid := bytes.Clone(body)
	invalid[len(invalid)-1] = ']'
	padded := append(bytes.Clone(body), '\t')
	nonstream := bytes.Replace(body, []byte(`"stream":true`), []byte(`"stream":false`), 1)
	typed := append(bytes.Clone(body[:len(body)-1]), `,"type":"response.create"}`...)
	reused := bytes.Clone(body)
	deep := []byte(`{"stream":true,"text":"` + strings.Repeat("x", validMemoMin) + `","input":` +
		strings.Repeat("[", jsonMaxDepth) + `0` + strings.Repeat("]", jsonMaxDepth) + `}`)
	for _, test := range []struct {
		name         string
		held         []byte
		selected     []byte
		want         []byte
		memoInvalid  bool
		leaveUnseen  bool
		warmHash     bool
		release      bool
		rewriteAfter []byte
		deferred     bool
	}{
		{name: "exact", held: body, selected: body, want: body},
		{name: "deferred plain", held: body, selected: body, want: body, deferred: true},
		{name: "exact nonstream", held: nonstream, selected: nonstream},
		{name: "exact explicit type", held: typed, selected: typed},
		{name: "not yet validated", held: body, selected: body, want: body, leaveUnseen: true},
		{name: "hash memo only", held: body, selected: body, want: body, leaveUnseen: true, warmHash: true},
		{name: "rewritten model", held: body, selected: rewritten, want: wantRewrite},
		{name: "same length invalid copy", held: body, selected: invalid},
		{name: "same address truncated", held: body, selected: body[:len(body)-1]},
		{name: "same address trimmed", held: padded, selected: padded, want: padded[:len(padded)-1]},
		{name: "released", held: body, selected: body, want: body, release: true},
		{name: "reused address after release", held: reused, selected: reused, release: true, rewriteAfter: invalid},
		{name: "exact negative", held: invalid, selected: invalid, memoInvalid: true},
		{name: "negative above memo depth", held: deep, selected: deep, want: deep, memoInvalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.warmHash && !jsonValid(bytes.Clone(test.held)) {
				t.Fatal("fixture could not warm the hash memo")
			}
			ctx, cancel := context.WithCancel(context.Background())
			holdBody(ctx, test.held)
			release := func() {
				cancel()
				waitHeldReleased(t, test.held)
			}
			t.Cleanup(release)
			if !sharesBodyListed(test.held) {
				t.Fatal("fixture did not hold its body")
			}
			if !test.leaveUnseen {
				entry, held := heldLongOf(test.held)
				if !held || entry.n != len(test.held) || entry.valid == test.memoInvalid {
					t.Fatalf("unexpected memo: held=%v, size=%d, valid=%v", held, entry.n, entry.valid)
				}
			}
			if test.release {
				release()
				if test.rewriteAfter != nil {
					copy(test.held, test.rewriteAfter)
				}
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if test.deferred {
				req = req.WithContext(context.WithValue(ctx, codexWireKey{}, &codexWireAttempt{plain: test.selected}))
			} else {
				provider.SetBody(req, test.selected)
			}
			message, ok := codexSocketMessage(req)
			if ok != (test.want != nil) || !bytes.Equal(message, test.want) {
				t.Fatalf("message length=%d, ok=%v, want length=%d, ok=%v", len(message), ok, len(test.want), test.want != nil)
			}
			if ok && &message[0] != &test.selected[0] {
				t.Fatal("copied selected replay bytes")
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
