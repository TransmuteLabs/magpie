package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

type p19HookReplyRun struct {
	calls   int
	reply   json.RawMessage
	request json.RawMessage
	success bool
}

func (run *p19HookReplyRun) Finish(_ context.Context, reply json.RawMessage, success bool, request ...json.RawMessage) {
	run.calls++
	run.reply, run.success = reply, success
	if len(request) == 1 {
		run.request = request[0]
	}
}

func (*p19HookReplyRun) Close() {}

func p19HookCompletedSSE(reply string) string {
	return "data: {\"type\":\"response.completed\",\"response\":" + reply + "}\n\n"
}

func TestGatewayHookReplyP19Parity(t *testing.T) {
	const done = `{"id":"r1","status":"completed","output":[]}`
	const failed = `{"status":"failed"}`
	const incomplete = `{"status":"incomplete"}`
	const duplicateError = `{"error":{"message":"bad"},"\u0065rror":null}`
	const reverseError = `{"error":null,"\u0065rror":{"message":"bad"}}`
	const duplicateStatus = `{"status":"completed","\u0073tatus":"failed"}`
	const reverseStatus = `{"status":"failed","\u0073tatus":"completed"}`
	const uppercaseStatus = `{"Status":"failed","Error":{"message":"bad"},"status":"completed"}`
	const duplicatePayload = `{"item":1,"item":2,"nested":[{"test":"some text"}]}`
	const invalidUTF8 = "{\"note\":\"\xff\"}"
	const malformedSSE = "data: {\"type\":\n\n"
	const partialSSE = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"a\"}\n\n"
	for _, test := range []struct {
		name, body, reply, replyError string
		success, stream               bool
		httpStatus                    int
	}{
		{name: "empty object", body: " \t{}\r\n", reply: "{}", success: true},
		{name: "object", body: "\r\n " + done + "\t", reply: done, success: true},
		{name: "failed status", body: failed, reply: failed},
		{name: "incomplete status", body: incomplete, reply: incomplete},
		{name: "duplicate error last null", body: duplicateError, reply: duplicateError, success: true},
		{name: "duplicate error last object", body: reverseError, reply: reverseError},
		{name: "duplicate status last failed", body: duplicateStatus, reply: duplicateStatus},
		{name: "duplicate status last completed", body: reverseStatus, reply: reverseStatus, success: true},
		{name: "status and error case sensitive", body: uppercaseStatus, reply: uppercaseStatus, success: true},
		{name: "duplicate payload preserved", body: duplicatePayload, reply: duplicatePayload, success: true},
		{name: "invalid UTF8 preserved", body: invalidUTF8, reply: invalidUTF8, success: true},
		{name: "array", body: "[{}]", replyError: "invalid model reply"},
		{name: "null", body: "null", replyError: "invalid model reply"},
		{name: "string", body: `"{}"`, replyError: "invalid model reply"},
		{name: "number", body: "1e2", replyError: "invalid model reply"},
		{name: "boolean", body: "false", replyError: "invalid model reply"},
		{name: "malformed JSON", body: "{", replyError: "incomplete model stream"},
		{name: "blank", body: " \r\n\t", replyError: "incomplete model stream"},
		{name: "completed SSE", body: partialSSE + p19HookCompletedSSE(done), reply: done, success: true, stream: true},
		{name: "interrupted SSE", body: partialSSE, replyError: "incomplete model stream", stream: true},
		{name: "malformed SSE", body: malformedSSE, replyError: "invalid model stream", stream: true},
		{name: "HTTP failure", body: done, reply: done, httpStatus: http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := []byte(test.body)
			reply, err := gatewayReply(provider.Responses, input)
			switch {
			case test.replyError != "":
				if err == nil || err.Error() != test.replyError || reply != nil {
					t.Fatalf("reply = %q, error = %v, want no reply and %q", reply, err, test.replyError)
				}
			case err != nil:
				t.Fatal(err)
			default:
				clear(input)
				if string(reply) != test.reply {
					t.Fatalf("owned reply = %q, want %q", reply, test.reply)
				}
			}

			status := test.httpStatus
			if status == 0 {
				status = http.StatusOK
			}
			requestBody := []byte(`{"model":"fixture","stream":false}`)
			if test.stream {
				requestBody = []byte(`{"model":"fixture","stream":true}`)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", nil)
			client := httptest.NewRecorder()
			run := &p19HookReplyRun{}
			writer, finish := gatewayResponse(client, req, provider.Responses, requestBody, run)
			writer.WriteHeader(status)
			if n, err := writer.Write([]byte(test.body)); err != nil || n != len(test.body) {
				t.Fatalf("write = (%d, %v), want %d bytes", n, err, len(test.body))
			}
			if run.calls != 0 || client.Code != status || client.Body.String() != test.body {
				t.Fatalf("before finish: calls = %d, HTTP status = %d, client bytes = %q", run.calls, client.Code, client.Body.Bytes())
			}
			finish()
			wantReply := test.reply
			if status >= 400 {
				wantReply = ""
			}
			if run.calls != 1 || run.success != test.success || string(run.reply) != wantReply || !bytes.Equal(run.request, requestBody) {
				t.Fatalf("finish: calls = %d, success = %v, reply = %q, request = %q", run.calls, run.success, run.reply, run.request)
			}
			if test.stream && !client.Flushed {
				t.Fatal("finish did not flush the streaming client")
			}
		})
	}
}

func p19HookReplyBody(size int) string {
	const prefix = `{"id":"r1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"`
	const suffix = `"}]}]}`
	return prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
}

func BenchmarkGatewayHookReplyP19(b *testing.B) {
	for _, shape := range []struct {
		name   string
		size   int
		stream bool
	}{
		{name: "unary/4KiB", size: 4 << 10},
		{name: "unary/256KiB", size: 256 << 10},
		{name: "unary/4MiB", size: 4 << 20},
		{name: "sse/completed4KiB", size: 4 << 10, stream: true},
	} {
		reply := p19HookReplyBody(shape.size)
		wire := []byte(reply)
		reqBody := []byte(`{"model":"fixture","stream":false}`)
		if shape.stream {
			wire = []byte(p19HookCompletedSSE(reply))
			reqBody = []byte(`{"model":"fixture","stream":true}`)
		}
		b.Run("classify/"+shape.name, func(b *testing.B) {
			parsed, err := gatewayReply(provider.Responses, wire)
			if err != nil || string(parsed) != reply {
				b.Fatal("classify returned different reply bytes", err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(wire)))
			for b.Loop() {
				parsed, err := gatewayReply(provider.Responses, wire)
				if err != nil || len(parsed) != shape.size {
					b.Fatal("classify returned a different reply", err)
				}
			}
		})
		b.Run("capture/"+shape.name, func(b *testing.B) {
			req := httptest.NewRequestWithContext(b.Context(), http.MethodPost, "/v1/responses", nil)
			client := httptest.NewRecorder()
			client.Body = nil
			run := &p19HookReplyRun{}
			capture := func() {
				*run = p19HookReplyRun{}
				writer, finish := gatewayResponse(client, req, provider.Responses, reqBody, run)
				n, err := writer.Write(wire)
				finish()
				if err != nil || n != len(wire) || run.calls != 1 || !run.success || len(run.reply) != shape.size {
					b.Fatal("capture returned a different reply", n, err)
				}
			}
			capture()
			if string(run.reply) != reply {
				b.Fatal("capture returned different reply bytes")
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(wire)))
			for b.Loop() {
				capture()
			}
		})
	}
}
