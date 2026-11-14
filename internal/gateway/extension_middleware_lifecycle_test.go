package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSDKMiddlewarePeerPublicationAndCancellation(test *testing.T) {
	directory := buildSDKMiddleware(test)
	for _, mutation := range []string{"disable", "config", "replacement", "cancel"} {
		test.Run(mutation, func(test *testing.T) {
			fresh(test)
			started := filepath.Join(test.TempDir(), "started")
			release := filepath.Join(test.TempDir(), "release")
			manager := setupSDKMiddleware(test, directory, sdkMiddlewareDescriptor(),
				map[string]any{"mode": "hold", "started": started, "release": release})
			peer := extension.NewManager(manager.Namespace())
			test.Cleanup(func() { closeSDKGatewayHook(test, peer) })
			ctx, cancel := context.WithCancel(test.Context())
			runs := manager.BeginMiddleware(ctx, sdk.GatewayContext{Origin: "local", Protocol: "chat"})
			if len(runs) != 1 {
				test.Fatal("middleware descriptor not admitted")
			}
			finished := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, err := runs[0].Apply(sdk.MethodMiddlewareRequest, []byte(`{"model":"fake/m1"}`), sdk.MiddlewareContext{})
				finished <- err
			}()
			test.Cleanup(func() {
				cancel()
				_ = os.WriteFile(release, []byte("release"), 0600)
				runs[0].Close()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					test.Error("middleware call outlived its request")
				}
			})
			waitSDKSearchMarker(test, started)
			switch mutation {
			case "disable":
				if err := peer.SetEnabled(test.Context(), "middleware.fixture", false); err != nil {
					test.Fatal(err)
				}
			case "config":
				configureSDKMiddleware(test, peer, map[string]any{})
			case "replacement":
				installSDKMiddleware(test, peer, directory, "2.0.0", sdkMiddlewareDescriptor())
			case "cancel":
				cancel()
			}
			if mutation != "cancel" && !manager.Running("middleware.fixture") {
				test.Fatal("peer mutation stopped the worker before publication")
			}
			if mutation == "cancel" {
				waitSDKSearchMarker(test, started+".cancelled")
			} else if err := os.WriteFile(release, []byte("release"), 0600); err != nil {
				test.Fatal(err)
			}
			select {
			case err := <-finished:
				if err == nil || mutation != "cancel" && !strings.Contains(err.Error(), "changed") {
					test.Fatalf("publication fence did not reject the exact peer mutation: %v", err)
				}
			case <-time.After(5 * time.Second):
				test.Fatal("middleware call did not finish")
			}
		})
	}
}

func TestSDKMiddlewareDefaultOriginAndDisableStayCold(test *testing.T) {
	fresh(test)
	directory := buildSDKMiddleware(test)
	trace := filepath.Join(test.TempDir(), "trace")
	manager := setupSDKMiddleware(test, directory, sdkMiddlewareDescriptor(), map[string]any{"trace": trace})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"c","model":"m1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	if err := provider.Save(provider.Provider{ID: "fake", Name: "Fake", Key: "k", Models: []string{"m1"}, Chat: upstream.URL + "/v1"}); err != nil {
		test.Fatal(err)
	}
	_, secrets := newCaller(test, "Another machine")
	server := New()
	response := sdkMiddlewareFront(server, `{"model":"fake/m1","messages":[{"role":"user","content":"hello"}]}`, secrets[0])
	if response.Code != 200 || manager.Running("middleware.fixture") || len(readSDKMiddlewareTrace(test, trace)) != 0 {
		test.Fatalf("default middleware disclosed a gateway-key request: %d %s", response.Code, response.Body.String())
	}
	if err := manager.SetEnabled(test.Context(), "middleware.fixture", false); err != nil {
		test.Fatal(err)
	}
	response = sdkMiddlewareFront(server, `{"model":"fake/m1","messages":[{"role":"user","content":"hello"}]}`, "magpie")
	if response.Code != 200 || calls.Load() != 2 || manager.Running("middleware.fixture") || len(readSDKMiddlewareTrace(test, trace)) != 0 {
		test.Fatalf("disabled middleware started or disrupted ordinary routing: %d %s", response.Code, response.Body.String())
	}
}
