package gateway

import (
	"bytes"
	"io"
	"net/http"

	builtincodex "github.com/yetone/magpie/internal/extension/builtin/codex"
)

// codexDropped handles the relay's drop check on its host-owned reply path.
func codexDropped(res *http.Response, size int) *http.Response {
	if res == nil {
		return nil
	}
	return codexDropReply(res, builtincodex.ReadResponsesDrop(res.StatusCode, size, res.Body))
}

func codexDropReply(res *http.Response, reply builtincodex.ResponsesWireReply) *http.Response {
	if reply.ResetJSON == nil {
		res.Body = reply.Body
		return res
	}
	h := res.Header.Clone()
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Encoding", "Transfer-Encoding"} {
		h.Del(k)
	}
	h.Set("Content-Type", "application/json")
	out := *res
	out.StatusCode, out.Status = http.StatusServiceUnavailable, "503 Service Unavailable"
	out.Header, out.Body, out.ContentLength = h, io.NopCloser(bytes.NewReader(reply.ResetJSON)), int64(len(reply.ResetJSON))
	return &out
}

func edgeMayDrop(status, size int) bool { return builtincodex.EdgeMayDrop(status, size) }

func edgeSaidDrop(status int, head []byte) bool { return builtincodex.EdgeSaidDrop(status, head) }

func codexReset(status int, body []byte) bool { return builtincodex.ResponseReset(status, body) }
