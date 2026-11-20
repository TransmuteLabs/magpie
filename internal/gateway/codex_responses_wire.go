package gateway

import (
	"errors"
	"io"
	"net/http"

	builtincodex "github.com/yetone/magpie/internal/extension/builtin/codex"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var linkedCodexResponses = builtincodex.ExecuteResponsesWire

func (s *Server) sendLinkedCodexResponses(p provider.Provider, req *http.Request, body []byte) (*http.Response, int, error) {
	ctx := req.Context()
	return sendCodexWire(req, body, builtincodex.ResponsesWireRoutedTurn, func(sent int) (*http.Response, error) {
		sentUp(ctx, sent, len(body))
		res, err := p.Do(s.client, req)
		return gotDown(ctx, res), err
	})
}

func sendCodexRelay(req *http.Request, body []byte, send func(int) (*http.Response, error)) (*http.Response, int, error) {
	if req.Method == http.MethodPost {
		switch {
		case provider.CodexTurnURL(req.URL):
			return sendCodexWire(req, body, builtincodex.ResponsesWireRelayedTurn, send)
		case provider.CodexCompactURL(req.URL):
			return sendCodexWire(req, body, builtincodex.ResponsesWireNativeCompact, send)
		}
	}
	res, err := send(len(body))
	return res, len(body), err
}

func sendCodexWire(req *http.Request, body []byte, mode builtincodex.ResponsesWireMode, send func(int) (*http.Response, error)) (*http.Response, int, error) {
	var res *http.Response
	sent := len(body)
	dispatched := false
	reply, err := linkedCodexResponses(sdk.ExecuteRequest{
		Protocol: string(provider.Responses),
		Model:    modelOf(body),
		Body:     body,
	}, req.Header.Get("Content-Encoding"), mode, func(zstdBody []byte) (int, io.ReadCloser, error) {
		if dispatched {
			return 0, nil, errors.New("Codex wire attempt was already dispatched")
		}
		dispatched = true
		if zstdBody != nil {
			provider.SetCodexZstdBody(req, zstdBody)
			sent = len(zstdBody)
		}
		var err error
		res, err = send(sent)
		if res == nil {
			return 0, nil, err
		}
		return res.StatusCode, res.Body, err
	})
	if err != nil {
		return res, sent, err
	}
	if res == nil {
		return nil, sent, errors.New("Codex wire attempt returned no response")
	}
	return codexDropReply(res, reply), sent, nil
}
