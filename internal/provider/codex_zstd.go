package provider

import (
	"net/http"
	"net/url"
	"strings"

	builtincodex "github.com/yetone/magpie/internal/extension/builtin/codex"
)

// Codex's own client sends a ChatGPT account's turns to the backend
// zstd-compressed, and nothing else: read in openai/codex at a9b08151,
// codex-rs/core/src/client.rs responses_request_compression (the
// enable_request_compression feature, on by default in
// codex-rs/features/src/lib.rs, a ChatGPT sign-in, the OpenAI provider)
// and codex-rs/http-client/src/request.rs, which compresses the body at
// level 3 and sets Content-Encoding: zstd. Only POST /responses is: an API
// key, another provider, /responses/compact and /models go plain.
//
// ylorn (X, v0.1.1202): a text-only gpt-6-luna turn of 1100224 bytes sent
// plain was answered 503, the same turn zstd-compressed (259 bytes) 200,
// and through magpie, which decoded Codex's zstd and sent the backend
// plain JSON, 503 again.

// CodexZstdForTests sets whether turns to the ChatGPT backend go
// compressed, for a test, and returns what puts it back.
func CodexZstdForTests(on bool) (restore func()) {
	return builtincodex.ResponsesZstdForTests(on)
}

// CodexCompressed sends req's body, body, zstd-compressed as Codex's own
// client sends it, when req is a turn to the ChatGPT backend
// (CodexBase/responses), and as it is otherwise. It is called once the
// body is final and signed, and returns the bytes that go on the wire. The
// body is still the plain body everywhere magpie keeps or shows it.
func CodexCompressed(req *http.Request, body []byte) int {
	if req.Method != http.MethodPost || !codexTurnURL(req.URL) {
		return len(body)
	}
	if zstdBody := builtincodex.EncodeResponsesZstd(body, req.Header.Get("Content-Encoding")); zstdBody != nil {
		SetCodexZstdBody(req, zstdBody)
		return len(zstdBody)
	}
	return len(body)
}

func SetCodexZstdBody(req *http.Request, z []byte) {
	SetBody(req, z)
	req.Header.Del("Content-Length")
	req.Header.Set("Content-Encoding", "zstd")
}

// codexTurnURL is CodexBase's /responses, where Codex sends a turn.
func codexTurnURL(u *url.URL) bool {
	return codexEndpointURL(u, "/responses")
}

// CodexTurnURL says whether u is CodexBase's /responses, where Codex sends
// a turn.
func CodexTurnURL(u *url.URL) bool { return codexTurnURL(u) }

func CodexCompactURL(u *url.URL) bool { return codexEndpointURL(u, "/responses/compact") }

func codexEndpointURL(u *url.URL, endpoint string) bool {
	base, err := url.Parse(CodexBase)
	return err == nil && u != nil && strings.EqualFold(u.Host, base.Host) && u.Path == base.Path+endpoint
}
