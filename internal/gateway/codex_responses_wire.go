package gateway

import (
	"context"
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
	return sendCodexWire(s.client, req, body, builtincodex.ResponsesWireRoutedTurn, func(req *http.Request, sent int) (*http.Response, error) {
		sentUp(ctx, sent, len(body))
		res, err := p.Do(s.client, req)
		return gotDown(ctx, res), err
	})
}

func sendCodexRelay(client *http.Client, req *http.Request, body []byte, send func(*http.Request, int) (*http.Response, error)) (*http.Response, int, error) {
	if req.Method == http.MethodPost {
		switch {
		case provider.CodexTurnURL(req.URL):
			return sendCodexWire(client, req, body, builtincodex.ResponsesWireRelayedTurn, send)
		case provider.CodexCompactURL(req.URL):
			return sendCodexWire(client, req, body, builtincodex.ResponsesWireNativeCompact, send)
		}
	}
	res, err := send(req, len(body))
	return res, len(body), err
}

func sendCodexWire(client *http.Client, req *http.Request, body []byte, mode builtincodex.ResponsesWireMode, send func(*http.Request, int) (*http.Response, error)) (*http.Response, int, error) {
	var res *http.Response
	sent := len(body)
	dispatched := false
	reply, err := linkedCodexResponses(sdk.ExecuteRequest{
		Protocol: string(provider.Responses),
		Model:    modelOf(body),
		Body:     body,
	}, req.Header.Get("Content-Encoding"), mode, func(encode func() []byte) (int, int, io.ReadCloser, error) {
		if dispatched {
			return 0, 0, nil, errors.New("Codex wire attempt was already dispatched")
		}
		dispatched = true
		var err error
		if encode != nil && deferCodexWire(client, req) {
			wire := &codexWireAttempt{plain: body, encode: encode}
			res, err = send(req.WithContext(context.WithValue(req.Context(), codexWireKey{}, wire)), 0)
			sent = wire.sent
			wire.plain, wire.encoded, wire.encode = nil, nil, nil
			if !wire.http && res != nil && (res.StatusCode == http.StatusServiceUnavailable || res.StatusCode == http.StatusInsufficientStorage) {
				sent = len(body)
				if zstdBody := encode(); zstdBody != nil {
					sent = len(zstdBody)
				}
			}
		} else {
			if encode != nil {
				if zstdBody := encode(); zstdBody != nil {
					provider.SetCodexZstdBody(req, zstdBody)
					sent = len(zstdBody)
				}
			}
			res, err = send(req, sent)
		}
		if res == nil {
			return 0, sent, nil, err
		}
		return res.StatusCode, sent, res.Body, err
	})
	if err != nil {
		return res, sent, err
	}
	if res == nil {
		return nil, sent, errors.New("Codex wire attempt returned no response")
	}
	return codexDropReply(res, reply), sent, nil
}

type codexWireKey struct{}

type codexWireAttempt struct {
	plain   []byte
	encode  func() []byte
	encoded []byte
	sent    int
	http    bool
}

func codexWireOf(req *http.Request) *codexWireAttempt {
	wire, _ := req.Context().Value(codexWireKey{}).(*codexWireAttempt)
	return wire
}

func deferCodexWire(client *http.Client, req *http.Request) bool {
	if client == nil || req.Header.Get("Content-Encoding") != "" {
		return false
	}
	if turn, _ := codexSocketTurn(req); !turn {
		return false
	}
	if _, drawing := drawWaitOf(req.Context()); drawing {
		return false
	}
	transport := client.Transport
	if metered, ok := transport.(rpmTransport); ok {
		transport = metered.next
	}
	if split, ok := transport.(drawsApart); ok {
		_, regular := split.any.(codexSocket)
		_, uploading := split.uploads.(codexSocket)
		return regular && uploading
	}
	_, ok := transport.(codexSocket)
	return ok
}

func (wire *codexWireAttempt) httpRequest(req *http.Request) *http.Request {
	if !wire.http {
		wire.http = true
		wire.encoded = wire.encode()
		wire.sent = len(wire.plain)
		if wire.encoded != nil {
			wire.sent = len(wire.encoded)
		}
		if count := upBytesOf(req.Context()); count != nil {
			count.addUp(wire.sent)
			count.last += wire.sent
		}
	}
	if wire.encoded == nil {
		return req
	}
	if req.Body != nil {
		req.Body.Close()
	}
	out := req.Clone(req.Context())
	provider.SetCodexZstdBody(out, wire.encoded)
	return out
}
