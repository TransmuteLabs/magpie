package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/yetone/magpie/pkg/protocolext"
)

func TestHTTPErrorStatusUsesExistingBoundedWireCodes(t *testing.T) {
	for status := 400; status <= 599; status++ {
		public := NewHTTPError(status, "safe rejection")
		wire, err := json.Marshal(publicError(public))
		if err != nil {
			t.Fatal(err)
		}
		var decoded Error
		if err := json.Unmarshal(wire, &decoded); err != nil || decoded.HTTPStatus() != status ||
			decoded.Message != "safe rejection" {
			t.Fatalf("HTTP status %d did not survive RPC: %s, %v", status, wire, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(wire, &fields); err != nil || len(fields) != 2 || fields["code"] == nil || fields["message"] == nil {
			t.Fatal("HTTP rejection changed the RPC error schema", string(wire), err)
		}
	}
	if got := NewHTTPError(400, "bad input"); got.Code != "invalid_params" {
		t.Fatal("400 lost its existing error code", got)
	}
	for _, status := range []int{-1, 0, 200, 302, 399, 600, 999} {
		if got := NewHTTPError(status, "safe rejection"); got.HTTPStatus() != 400 {
			t.Fatalf("invalid rejection status %d granted HTTP %d", status, got.HTTPStatus())
		}
	}
	for _, code := range []string{
		"http.200", "http.302", "http.399", "http.600", "http.0504", "http.504.extra",
		"http.+504", "http.5e2", "HTTP.504", "http.٥٠٤", "http. 504", "vendor.504",
	} {
		if got := (&Error{Code: code, Message: "safe"}).HTTPStatus(); got != 0 {
			t.Fatalf("malformed HTTP code %q selected status %d", code, got)
		}
	}
	for _, test := range []struct {
		code   string
		status int
	}{{"canceled", 499}, {"deadline", 504}, {"resource_exhausted", 413}, {"invalid_params", 400}, {"internal", 0}} {
		if got := (&Error{Code: test.code}).HTTPStatus(); got != test.status {
			t.Fatalf("%s status = %d; want %d", test.code, got, test.status)
		}
	}
	if (*Error)(nil).HTTPStatus() != 0 {
		t.Fatal("nil error selected a rejection status")
	}
	for _, message := range []string{"", " \t\r\n", "unsafe\x00message", strings.Repeat("x", 4097)} {
		if got := NewHTTPError(504, message); got.Code != "internal" || got.Message != "extension handler failed" {
			t.Fatal("invalid public rejection message escaped", got)
		}
	}
}

type rejectingInbound struct {
	err error
}

func (rejectingInbound) ID() string   { return "rejection" }
func (rejectingInbound) Path() string { return "/fixture.errors/Reject" }
func (rejectingInbound) Contract() protocolext.Contract {
	return protocolext.InboundContract()
}
func (codec rejectingInbound) Decode(context.Context, http.Header, []byte) (protocolext.Request, error) {
	return protocolext.Request{}, codec.err
}
func (rejectingInbound) Reply(writer http.ResponseWriter) protocolext.Response {
	return rejectionResponse{writer}
}

type rejectionResponse struct{ http.ResponseWriter }

func (rejectionResponse) Finish(error) error { return nil }

func TestInboundCodecRejectionsPreserveHTTPStatusAndSafeMessage(t *testing.T) {
	for _, status := range []int{400, 413, 415, 504} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			codec := rejectingInbound{err: fmt.Errorf("private credential wrapper: %w", &protocolext.Error{
				Status: status, Message: "safe codec rejection",
			})}
			module := Module{
				Manifest: testManifest(InboundCodec),
				Inbounds: []protocolext.Inbound{codec},
			}
			module.Manifest.Inbounds = []InboundDescriptor{{ID: codec.ID(), Path: codec.Path()}}
			peer := startPeer(t, module)
			result, events := completeRPC(t, peer, callFrame("decode", MethodInboundDecode,
				NativeRequest{ID: codec.ID(), ResponseID: "response"}))
			if result.Error == nil || result.Error.HTTPStatus() != status || result.Error.Message != "safe codec rejection" || len(events) != 0 {
				t.Fatalf("codec rejection changed across SDK: result=%+v events=%+v", result, events)
			}
			if strings.Contains(result.Error.Error(), "credential") {
				t.Fatal("private wrapper escaped the codec's public message")
			}
		})
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errors.New("private credential")} {
		t.Run(fmt.Sprintf("%T/%s", cause, cause), func(t *testing.T) {
			codec := rejectingInbound{err: cause}
			module := Module{Manifest: testManifest(InboundCodec), Inbounds: []protocolext.Inbound{codec}}
			module.Manifest.Inbounds = []InboundDescriptor{{ID: codec.ID(), Path: codec.Path()}}
			result, _ := completeRPC(t, startPeer(t, module), callFrame("decode", MethodInboundDecode,
				NativeRequest{ID: codec.ID(), ResponseID: "response"}))
			if result.Error == nil || strings.Contains(result.Error.Message, "credential") {
				t.Fatal("context/private error was exposed or accepted", result)
			}
			expected := 0
			if errors.Is(cause, context.Canceled) {
				expected = 499
			} else if errors.Is(cause, context.DeadlineExceeded) {
				expected = 504
			}
			if result.Error.HTTPStatus() != expected {
				t.Fatalf("context/private classification changed: %+v", result.Error)
			}
		})
	}
}
