package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionToolAccountRecheckedAfterWorkerQueue(test *testing.T) {
	for _, change := range []string{"unchanged", "login off", "account replaced", "configuration", "discovery off"} {
		test.Run(change, func(test *testing.T) {
			fixture := installToolRefreshFixture(test, nil)
			ctx, cancel := context.WithTimeout(test.Context(), 30*time.Second)
			defer cancel()
			requestID := randomToken()
			ctx = extension.WithRequestMetadata(ctx, extension.RequestMetadata{Plugin: fixture.scope.Plugin, RequestID: requestID})
			identity, _ := json.Marshal([]string{fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account, "", "", requestID})
			operation := fmt.Sprintf("request-%x", sha256.Sum256(identity))
			blockerPrefix := "hold-" + requestID + "-"
			count := sdk.MaxConcurrent/2 - 2
			occupied := make(chan struct{}, count)
			released := make(chan struct{})
			release := sync.OnceFunc(func() { close(released) })
			var selectedCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var call sdk.Call
				if request.Method != http.MethodPost || request.URL.Path != "/tool" || json.NewDecoder(request.Body).Decode(&call) != nil ||
					call.Scope.Plugin != fixture.scope.Plugin {
					http.NotFound(writer, request)
					return
				}
				switch {
				case call.Scope.Account == fixture.scope.Account && call.Scope.Operation == operation:
					selectedCalls.Add(1)
				case call.Scope.Account == "blocker" && strings.HasPrefix(call.Scope.Operation, blockerPrefix):
					occupied <- struct{}{}
					select {
					case <-released:
					case <-request.Context().Done():
						return
					}
				default:
					http.NotFound(writer, request)
					return
				}
				writer.WriteHeader(http.StatusOK)
			}))
			test.Cleanup(upstream.Close)
			account, err := fixture.manager.Store().GetAccountContext(ctx, fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
			if err != nil {
				test.Fatal(err)
			}
			account.Auth, _ = json.Marshal(map[string]string{"url": upstream.URL})
			account.ExpiresAt = 0
			account, err = fixture.manager.Store().SaveAccount(account, account.Generation)
			if err != nil {
				test.Fatal(err)
			}
			blocker, err := fixture.manager.Store().SaveAccount(extension.Account{
				Plugin: account.Plugin, Provider: account.Provider, ID: "blocker", Auth: account.Auth,
			}, 0)
			if err != nil {
				test.Fatal(err)
			}
			provider.Changed()
			scope := fixture.scope
			scope.Generation, scope.Operation, scope.Attempt = account.Generation, operation, 1
			blocked := make(chan error, count)
			var workers sync.WaitGroup
			defer func() {
				cancel()
				release()
				workers.Wait()
			}()
			for index := range count {
				workers.Go(func() {
					_, err := fixture.manager.Call(ctx, account.Plugin, sdk.MethodToolCall, sdk.Scope{
						Provider: blocker.Provider, Account: blocker.ID, Generation: blocker.Generation,
						Operation: blockerPrefix + strconv.Itoa(index), Attempt: 1,
					}, sdk.ToolRequest{ID: "echo", Arguments: json.RawMessage(`{}`)}, nil)
					blocked <- err
				})
			}
			for range count {
				select {
				case <-occupied:
				case err := <-blocked:
					test.Fatalf("slot holder returned before release: %v", err)
				case <-ctx.Done():
					test.Fatal("worker slots did not fill")
				}
			}
			request := httptest.NewRequest(http.MethodPost, ToolsMCPPath, strings.NewReader(toolsCall(fixture.tool.name, string(refreshToolArgs)))).WithContext(ctx)
			request.RemoteAddr, request.Host = "127.0.0.1:50000", "127.0.0.1:3425"
			request.Header.Set("Content-Type", "application/json")
			handler := lanGuard(New().Handler())
			result := make(chan *httptest.ResponseRecorder, 1)
			workers.Go(func() {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				result <- response
			})
			poll := time.NewTicker(5 * time.Millisecond)
			defer poll.Stop()
			for {
				receipt, err := fixture.manager.Store().OperationContext(ctx, scope.Plugin, scope.Operation)
				if err == nil {
					if receipt.Scope != scope || receipt.State != "dispatched" || selectedCalls.Load() != 0 {
						test.Fatalf("tool was not queued after journaling: %+v, calls=%d", receipt, selectedCalls.Load())
					}
					break
				}
				var missing *sdk.Error
				if !errors.As(err, &missing) || missing.Code != "not_found" {
					test.Fatal(err)
				}
				select {
				case response := <-result:
					test.Fatalf("tool returned before journaling: %s", response.Body)
				case <-ctx.Done():
					test.Fatal("tool never reached the queue")
				case <-poll.C:
				}
			}

			switch change {
			case "login off":
				err = provider.SetLoginOn(provider.ExtensionProviderID(scope.Plugin, scope.Provider), scope.Account, false)
			case "account replaced":
				account.Auth = json.RawMessage(`{"token":"replacement"}`)
				_, err = fixture.manager.Store().SaveAccount(account, account.Generation)
			case "configuration":
				peer := extension.NewManager(fixture.manager.Namespace())
				test.Cleanup(func() {
					if err := peer.Close(context.Background()); err != nil {
						test.Error(err)
					}
				})
				_, err = peer.CompareAndSwapModuleConfig(ctx, scope.Plugin, "0", json.RawMessage(`{"updated":true}`))
			case "discovery off":
				current := settings.Load()
				current.NoAccountDiscovery = true
				err = settings.Save(current)
			}
			if err != nil {
				test.Fatal(err)
			}
			release()
			select {
			case response := <-result:
				reply := readMCP(test, response)
				if reply.Result.IsError != (change != "unchanged") {
					test.Errorf("unexpected MCP outcome: %s", response.Body)
				}
			case <-ctx.Done():
				test.Fatal("queued tool did not finish")
			}
			wantCalls, wantState := int32(0), "not-started"
			if change == "unchanged" {
				wantCalls, wantState = 1, "completed"
			}
			receipt, err := fixture.manager.Store().OperationContext(ctx, scope.Plugin, scope.Operation)
			if err != nil || receipt.Scope != scope || receipt.State != wantState || selectedCalls.Load() != wantCalls {
				test.Fatalf("tool sent %d RPCs and journaled %q, want %d and %q: %v", selectedCalls.Load(),
					receipt.State, wantCalls, wantState, err)
			}
		})
	}
}
