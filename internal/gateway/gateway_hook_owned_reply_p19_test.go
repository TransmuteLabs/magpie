package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSDKGatewayCapturedReplyP19(t *testing.T) {
	fresh(t)
	deliveries := make(chan sdkHookDelivery, 1)
	observer := httptest.NewServer(withoutAnonymousFixtureProbe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var delivery sdkHookDelivery
		if err := json.NewDecoder(r.Body).Decode(&delivery); err != nil {
			http.Error(w, "invalid observer payload", http.StatusBadRequest)
			return
		}
		select {
		case deliveries <- delivery:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Error("duplicate observer callback")
			w.WriteHeader(http.StatusConflict)
		}
	})))
	t.Cleanup(observer.Close)
	manager := installSDKGatewayHook(t, sdkHookConfig{ResponseURL: observer.URL}, sdk.GatewayHookDescriptor{Response: true})
	const result = `{"id":"r1","status":"completed","output":[]}`
	const failed = `{"status":"failed","error":{"message":"fixture failure"}}`
	const statusFailed = `{"error":null,"status":"completed","\u0073tatus":"incompl\u0065te"}`
	const finalNullError = `{"error":{"message":"replaced"},"\u0065rror": null ,"Status":"failed","status":false}`
	const deliveredNullError = `{"error":{"message":"replaced"},"\u0065rror":null,"Status":"failed","status":false}`
	for _, test := range []struct {
		name, wire, wantReply string
		status                int
		captureLimit          int64
		stream, success       bool
		readError             bool
	}{
		{name: "unary", wire: " \t" + result + "\n", wantReply: result, status: 200, success: true},
		{name: "SSE terminal", wire: p19HookCompletedSSE(result), wantReply: result, status: 200, stream: true, success: true},
		{name: "failed response", wire: failed, wantReply: failed, status: 200},
		{name: "last decoded error member", wire: finalNullError, wantReply: deliveredNullError, status: 200, success: true},
		{name: "last decoded SSE status", wire: p19HookCompletedSSE(statusFailed), wantReply: statusFailed, status: 200, stream: true},
		{name: "truncated SSE", wire: `data: {"type":"response.output_text.delta","delta":"x"}` + "\n\n", wantReply: "null", status: 200, stream: true},
		{name: "HTTP failure", wire: result, wantReply: "null", status: 502},
		{name: "cut capture", wire: result, wantReply: "null", status: 200, captureLimit: 16},
		{name: "spool read error", wire: result, wantReply: "null", status: 200, readError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reqBody := `{"model":"fixture","stream":false,"input":"user"}`
			if test.stream {
				reqBody = `{"model":"fixture","stream":true,"input":"user"}`
			}
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			client := httptest.NewRecorder()
			writer, accepted, finish := applyGatewayHooks(client, req, provider.Responses, []byte(reqBody), manager.SelectGateway(ctx, "local"))
			capture, ok := writer.(*conversationWriter)
			if !ok {
				t.Fatal("SDK response hook did not capture the reply")
			}
			t.Cleanup(capture.full.discard)
			if test.captureLimit != 0 {
				capture.full.limit = test.captureLimit
			}
			writer.WriteHeader(test.status)
			wire := []byte(test.wire)
			if length, err := writer.Write(wire); err != nil || length != len(wire) {
				t.Fatalf("client write = (%d, %v), want %d bytes", length, err, len(wire))
			}
			if capture.full.f == nil {
				t.Fatal("missing spool file")
			}
			path := capture.full.f.Name()
			if test.readError {
				if err := capture.full.f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			clear(wire)
			cancel()
			finish()
			if _, err := os.Stat(path); capture.full.f != nil || !os.IsNotExist(err) {
				t.Fatalf("spool remained after finish: file = %v, stat = %v", capture.full.f, err)
			}
			clear(accepted)
			if client.Code != test.status || client.Body.String() != test.wire {
				t.Fatalf("client status/body = %d %q", client.Code, client.Body.Bytes())
			}
			delivery := receiveSDKHook(t, deliveries)
			observed := delivery.Response
			if observed == nil || observed.Success != test.success || string(observed.Request) != reqBody || string(observed.Reply) != test.wantReply {
				t.Fatalf("observer received %+v, want success %t and reply %s", observed, test.success, test.wantReply)
			}
			waitSDKHookCompleted(t, manager, delivery.Scope)
		})
	}
}

func BenchmarkSDKGatewayCapturedReplyP19(b *testing.B) {
	for _, shape := range []struct {
		name   string
		size   int
		stream bool
	}{
		{name: "unary4KiB", size: 4 << 10},
		{name: "unary1MiB", size: 1 << 20},
		{name: "sse1MiB", size: 1 << 20, stream: true},
	} {
		b.Run(shape.name, func(b *testing.B) {
			reply := p19HookReplyBody(shape.size)
			wire := []byte(reply)
			body := []byte(`{"model":"fixture","stream":false,"input":"user"}`)
			if shape.stream {
				wire = []byte(p19HookCompletedSSE(reply))
				body = []byte(`{"model":"fixture","stream":true,"input":"user"}`)
			}
			notices := make(chan int64, 1)
			observer := httptest.NewServer(withoutAnonymousFixtureProbe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				length, err := io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, 4<<20))
				if err != nil {
					http.Error(w, "invalid observer payload", http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				select {
				case notices <- length:
				default:
					b.Error("duplicate observer callback")
				}
			})))
			b.Cleanup(observer.Close)
			manager := extension.NewManager(b.TempDir())
			installSDKGatewayHookOn(b, manager, sdkHookConfig{ResponseURL: observer.URL}, sdk.GatewayHookDescriptor{Response: true})
			ctx := b.Context()
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", nil)
			info := sdk.GatewayContext{Origin: "local", Model: "fixture", Protocol: "responses", Stream: shape.stream}
			var foreground time.Duration
			capture := func() {
				run := manager.BeginGateway(ctx, body, info)
				if run == nil || !run.ObservesResponse() {
					b.Fatal("response-only SDK hook not admitted")
				}
				client := httptest.NewRecorder()
				client.Body = nil
				writer, done := gatewayResponse(client, req, provider.Responses, body, run)
				length, err := writer.Write(wire)
				if err != nil || length != len(wire) {
					run.Close()
					b.Fatal("captured write", length, err)
				}
				start := time.Now()
				done()
				foreground += time.Since(start)
				run.Close()
				wait := time.NewTimer(15 * time.Second)
				defer wait.Stop()
				select {
				case received := <-notices:
					if received < int64(len(reply)) {
						b.Fatalf("observer received only %d bytes of %d", received, len(reply))
					}
				case <-wait.C:
					b.Fatal("SDK observer did not receive the completed reply")
				}
			}
			capture()
			foreground = 0
			b.ReportAllocs()
			b.SetBytes(int64(shape.size))
			for b.Loop() {
				capture()
			}
			b.ReportMetric(float64(foreground.Nanoseconds())/float64(b.N), "ns/op-capture-finish")
		})
	}
}
