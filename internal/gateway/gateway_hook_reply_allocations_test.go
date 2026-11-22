package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestGatewayHookReplyP19UnaryAllocations(t *testing.T) {
	body := []byte(p19HookReplyBody(4 << 10))
	var reply json.RawMessage
	var err error
	allocations := testing.AllocsPerRun(20, func() {
		reply, err = gatewayReply(provider.Responses, body)
	})
	if err != nil || !bytes.Equal(reply, body) {
		t.Fatalf("gatewayReply did not preserve the unary reply: length %d, error %v", len(reply), err)
	}
	if allocations > 4 {
		t.Errorf("gatewayReply allocated %.0f times per unary reply, want at most 4", allocations)
	}
}

func TestGatewayHookCaptureP19MemberAllocations(t *testing.T) {
	const size = 4 << 10
	narrow := []byte(p19HookReplyBody(size))
	var fields strings.Builder
	for index := range 64 {
		fmt.Fprintf(&fields, `,"meta%d":"fixture"`, index)
	}
	reply := p19HookReplyBody(size - fields.Len())
	wide := []byte(reply[:len(reply)-1] + fields.String() + "}")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", nil)
	request := []byte(`{"model":"fixture","stream":false}`)
	allocations := func(wire []byte) float64 {
		recorder := httptest.NewRecorder()
		recorder.Body = nil
		run := &p19HookReplyRun{}
		return testing.AllocsPerRun(12, func() {
			*run = p19HookReplyRun{}
			writer, finish := gatewayResponse(recorder, req, provider.Responses, request, run)
			length, err := writer.Write(wire)
			finish()
			if err != nil || length != len(wire) || run.calls != 1 || !run.success || !bytes.Equal(run.reply, wire) {
				t.Fatalf("gateway capture did not deliver the original reply: wrote %d bytes, error %v, calls %d, success %t", length, err, run.calls, run.success)
			}
		})
	}
	plain, extra := allocations(narrow), allocations(wide)
	if extra > plain+8 {
		t.Errorf("extra reply members allocated %.0f vs %.0f times per captured reply of the same size", extra, plain)
	}
}
