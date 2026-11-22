package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
)

// gatewayResponse observes the bytes delivered to the client without changing
// them. The caller runs done after middleware has finished writing its reply.
func gatewayResponse(w http.ResponseWriter, r *http.Request, proto provider.Protocol, body []byte, run gatewayHookRun) (http.ResponseWriter, func()) {
	capture := &conversationWriter{captureResponseWriter: &captureResponseWriter{
		ResponseWriter: w, full: &spool{limit: conversationBodyMax, pattern: "magpie-hook-*"},
	}}
	return capture, func() {
		defer capture.full.discard()
		if streamOf(body) {
			_ = http.NewResponseController(w).Flush()
		}
		output, err := capture.full.read()
		// a client that hangs up once it has the whole reply (Codex, on
		// response.completed) still got it: gatewayReply judges the bytes
		if err != nil || capture.full.cut() || capture.status >= 400 {
			run.Finish(r.Context(), nil, false, body)
			return
		}
		reply, err := gatewayReply(proto, output)
		success := err == nil && gatewayReplySucceeded(reply)
		if sdkRun, ok := run.(*extension.GatewayRun); ok {
			sdkRun.FinishOwnedReply(r.Context(), reply, success, body)
			return
		}
		run.Finish(r.Context(), reply, success, body)
	}
}

func gatewayReplySucceeded(reply []byte) bool {
	reply = trimJSON(reply)
	if len(reply) == 0 || reply[0] != '{' {
		return true
	}
	var failure, status []byte
	eachMember(reply, func(key, value []byte) bool {
		switch {
		case keyIs(key, "error"):
			failure = value
		case keyIs(key, "status"):
			status = value
		}
		return true
	})
	if len(failure) != 0 && string(failure) != "null" {
		return false
	}
	state, _ := stringBytes(status)
	return string(state) != "failed" && string(state) != "incomplete"
}

// gatewayReply is the reply the client got as one whole reply in its API: a
// whole body as it is, and a stream read through the protocol's decoder into
// the reply the gateway would have rendered. A Responses stream's completed
// response is the reply itself. A stream that never ended is an error.
// The returned reply owns its buffer.
func gatewayReply(proto provider.Protocol, body []byte) (json.RawMessage, error) {
	body = bytes.TrimSpace(body)
	if json.Valid(body) {
		if body[0] != '{' {
			return nil, errors.New("invalid model reply")
		}
		return bytes.Clone(body), nil
	}
	var c collector
	var completed json.RawMessage
	decode := decoder(proto)
	ended := false
	err := readSSE(bytes.NewReader(body), func(_ string, data string) error {
		if data == "[DONE]" {
			ended = true
			return nil
		}
		if !json.Valid([]byte(data)) {
			return errors.New("invalid model stream")
		}
		if proto == provider.Responses {
			var e struct {
				Type     string          `json:"type"`
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal([]byte(data), &e) == nil && e.Type == "response.completed" && len(e.Response) > 0 {
				completed = e.Response
			}
		}
		return decode(data, func(e Event) {
			if e.Kind == KStop {
				ended = true
			}
			c.add(e)
		})
	})
	if err != nil {
		return nil, err
	}
	if !ended {
		return nil, errors.New("incomplete model stream")
	}
	if c.err != "" {
		return nil, errors.New(c.err)
	}
	if completed != nil {
		return completed, nil
	}
	res := c.finish()
	return render(proto, res, &Request{Model: res.Model}), nil
}
