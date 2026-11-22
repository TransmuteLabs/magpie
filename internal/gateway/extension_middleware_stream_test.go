package gateway

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gwmw"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func BenchmarkSDKMiddlewareSSE500(bench *testing.B) {
	manager := extension.NewManager(bench.TempDir())
	bench.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			bench.Error(err)
		}
	})
	installSDKMiddleware(bench, manager, buildSDKMiddleware(bench), "1.0.0", sdk.GatewayMiddlewareDescriptor{Event: true})
	configureSDKMiddleware(bench, manager, map[string]any{"mode": "transform"})
	firstLease, err := manager.Acquire(bench.Context(), "middleware.fixture")
	if err != nil {
		bench.Fatal(err)
	}
	firstLease.Close()
	event := []byte("event: delta\ndata: {\"chunk\":\"" + strings.Repeat("x", 2048) + "\"}\n\n")
	selection := manager.SelectGateway(bench.Context(), "local")
	agent := extensionMiddlewareAgent{selection: selection, ctx: bench.Context()}
	bench.ReportAllocs()
	bench.ReportMetric(500, "events/op")
	for bench.Loop() {
		run := agent.Begin(gwmw.Info{Protocol: "chat", Stream: true})
		if run == nil {
			bench.Fatal("missing SDK middleware")
		}
		response := httptest.NewRecorder()
		response.Header().Set("Content-Type", "text/event-stream")
		stream, finish := run.Wrap(response)
		for range 500 {
			if _, err := stream.Write(event); err != nil {
				run.End()
				bench.Fatal(err)
			}
		}
		finish()
		run.End()
		if got := bytes.Count(response.Body.Bytes(), []byte(`"sdkAccount":false`)); got != 500 {
			bench.Fatalf("transformed %d of 500 events", got)
		}
	}
}

func TestSDKMiddlewareStreamRevocation(test *testing.T) {
	directory := buildSDKMiddleware(test)
	for _, mutation := range []string{"config", "reenable"} {
		test.Run(mutation, func(test *testing.T) {
			manager := extension.NewManager(test.TempDir())
			peer := extension.NewManager(manager.Namespace())
			test.Cleanup(func() { closeSDKGatewayHook(test, manager) })
			test.Cleanup(func() { closeSDKGatewayHook(test, peer) })
			installSDKMiddleware(test, manager, directory, "1.0.0", sdk.GatewayMiddlewareDescriptor{Event: true})
			configureSDKMiddleware(test, manager, map[string]any{"mode": "transform"})
			agent := extensionMiddlewareAgent{selection: manager.SelectGateway(test.Context(), "local"), ctx: test.Context()}
			run := agent.Begin(gwmw.Info{Protocol: "chat", Stream: true})
			if run == nil {
				test.Fatal("missing SDK middleware")
			}
			defer run.End()
			response := httptest.NewRecorder()
			response.Header().Set("Content-Type", "text/event-stream")
			stream, finish := run.Wrap(response)
			defer finish()
			if _, err := stream.Write([]byte("event: delta\ndata: {\"chunk\":\"first\"}\n\n")); err != nil {
				test.Fatal(err)
			}
			if !strings.Contains(response.Body.String(), `"sdkAccount":false`) {
				test.Fatal("first event bypassed the selected SDK hook")
			}
			switch mutation {
			case "config":
				configureSDKMiddleware(test, peer, map[string]any{"mode": "drop"})
			case "reenable":
				for _, enabled := range []bool{false, true} {
					if err := peer.SetEnabled(test.Context(), "middleware.fixture", enabled); err != nil {
						test.Fatal(err)
					}
				}
			}
			boundary := response.Body.Len()
			second := "event: delta\ndata: {\"chunk\":\"second\"}\n\n"
			if _, err := stream.Write([]byte(second)); err != nil {
				test.Fatal(err)
			}
			if got := string(response.Body.Bytes()[boundary:]); got != second {
				test.Fatalf("revoked middleware changed the next event: %q", got)
			}
		})
	}
}
