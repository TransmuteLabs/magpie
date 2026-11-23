package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestExtensionToolAccountRecheckedAfterWorkerQueue(test *testing.T) {
	for _, change := range []string{"unchanged", "login off", "account replaced", "configuration", "same CAS configuration", "reordered configuration", "discovery off"} {
		test.Run(change, func(test *testing.T) {
			fixture := installToolRefreshFixture(test, nil)
			ctx, cancel := context.WithTimeout(test.Context(), 30*time.Second)
			defer cancel()
			if change == "same CAS configuration" || change == "reordered configuration" {
				if _, err := fixture.manager.CompareAndSwapModuleConfig(ctx, fixture.scope.Plugin, "0", json.RawMessage(toolConfigOriginal)); err != nil {
					test.Fatal(err)
				}
			}
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
			case "same CAS configuration", "reordered configuration":
				replaceToolConfigAtSameVersion(test, fixture, change)
			case "discovery off":
				current := settings.Load()
				current.NoAccountDiscovery = true
				err = settings.Save(current)
			}
			if err != nil {
				test.Fatal(err)
			}
			release()
			successful := change == "unchanged" || change == "reordered configuration"
			select {
			case response := <-result:
				reply := readMCP(test, response)
				if reply.Result.IsError == successful {
					test.Errorf("unexpected MCP outcome: %s", response.Body)
				}
			case <-ctx.Done():
				test.Fatal("queued tool did not finish")
			}
			wantCalls, wantState := int32(0), "not-started"
			if successful {
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

func TestExtensionToolAccountRecheckedAfterStdinDrains(test *testing.T) {
	worker := buildToolRefreshWorker(test)
	for _, change := range []string{"same CAS configuration", "reordered configuration"} {
		test.Run(change, func(test *testing.T) {
			fixture := installRefreshFixture(test, worker, sdk.FullContract().Version.Minor, time.Hour, nil)
			ctx, cancel := context.WithTimeout(test.Context(), 20*time.Second)
			var requests sync.WaitGroup
			defer func() {
				cancel()
				requests.Wait()
			}()
			if _, err := fixture.manager.CompareAndSwapModuleConfig(ctx, fixture.scope.Plugin, "0", json.RawMessage(toolConfigOriginal)); err != nil {
				test.Fatal(err)
			}
			resume := stallRefreshInput(test, ctx, fixture)
			requestID := randomToken()
			ctx = extension.WithRequestMetadata(ctx, extension.RequestMetadata{Plugin: fixture.scope.Plugin, RequestID: requestID})
			scope := fixture.scope
			identity, _ := json.Marshal([]string{scope.Plugin, scope.Provider, scope.Account, "", "", requestID})
			scope.Operation, scope.Attempt = fmt.Sprintf("request-%x", sha256.Sum256(identity)), 1
			request := httptest.NewRequest(http.MethodPost, ToolsMCPPath, strings.NewReader(toolsCall(fixture.tool.name, string(refreshToolArgs)))).WithContext(ctx)
			request.RemoteAddr, request.Host = "127.0.0.1:50000", "127.0.0.1:3425"
			request.Header.Set("Content-Type", "application/json")
			handler := lanGuard(New().Handler())
			result := make(chan *httptest.ResponseRecorder, 1)
			requests.Go(func() {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				result <- response
			})
			stacks := make([]byte, 1<<20)
			waitToolRefreshPhase(test, ctx, "account tool waiting to write to stdin", func() bool {
				for stack := range bytes.SplitSeq(stacks[:runtime.Stack(stacks, true)], []byte("\n\n")) {
					header, _, _ := bytes.Cut(stack, []byte("\n"))
					if bytes.Contains(header, []byte("[select]")) &&
						bytes.Contains(stack, []byte("(*Server).toolsMCPRun(")) &&
						(bytes.Contains(stack, []byte("(*workerClient).send(")) ||
							bytes.Contains(stack, []byte("(*workerClient).reserveWrite("))) {
						return true
					}
				}
				return false
			})
			receipt, err := fixture.manager.Store().OperationContext(ctx, scope.Plugin, scope.Operation)
			if err != nil || receipt.Scope != scope || receipt.State != "dispatched" || fixture.toolCalls.Load() != 0 {
				test.Fatalf("tool did not wait before dispatch: %+v, %d calls: %v", receipt, fixture.toolCalls.Load(), err)
			}
			replaceToolConfigAtSameVersion(test, fixture, change)
			resume()
			successful := change == "reordered configuration"
			select {
			case response := <-result:
				reply := readMCP(test, response)
				if reply.Result.IsError == successful {
					test.Errorf("unexpected MCP outcome: %s", response.Body)
				}
			case <-ctx.Done():
				test.Fatal("account tool did not finish after stdin drained")
			}
			wantCalls, wantState := int32(0), "not-started"
			if successful {
				wantCalls, wantState = 1, "completed"
			}
			receipt, err = fixture.manager.Store().OperationContext(ctx, scope.Plugin, scope.Operation)
			if err != nil || receipt.Scope != scope || receipt.State != wantState ||
				fixture.toolCalls.Load() != wantCalls || fixture.refreshes.Load() != 0 {
				test.Fatalf("tool sent %d RPCs and %d refreshes, journaled %q; want %d RPCs, 0 refreshes and %q: %v",
					fixture.toolCalls.Load(), fixture.refreshes.Load(), receipt.State, wantCalls, wantState, err)
			}
		})
	}
}

func TestExtensionToolHandoffRechecksAuthorityAfterStoreWait(test *testing.T) {
	worker := buildToolRefreshWorker(test)
	for _, change := range []string{
		"unchanged", "account signed out", "same generation credentials", "login off", "discovery off",
		"same CAS configuration", "reordered configuration",
	} {
		test.Run(change, func(test *testing.T) {
			fixture := installRefreshFixture(test, worker, sdk.FullContract().Version.Minor, time.Hour, nil)
			ctx, cancel := context.WithTimeout(test.Context(), 20*time.Second)
			var requests sync.WaitGroup
			original := runExtensionPluginTool
			defer func() {
				cancel()
				requests.Wait()
				runExtensionPluginTool = original
			}()
			if _, err := fixture.manager.CompareAndSwapModuleConfig(ctx, fixture.scope.Plugin, "0", json.RawMessage(toolConfigOriginal)); err != nil {
				test.Fatal(err)
			}
			entry, config, err := fixture.manager.ActiveModuleConfig(ctx, fixture.scope.Plugin)
			if err != nil {
				test.Fatal(err)
			}
			selection, err := extension.NewToolAccountSelection(fixture.manager.Namespace(), entry, config)
			if err != nil {
				test.Fatal(err)
			}
			resume := stallRefreshInput(test, ctx, fixture)
			reached := make(chan sdk.Scope, 1)
			proceed := make(chan struct{})
			runExtensionPluginTool = func(current context.Context, spec, name string, args json.RawMessage) (sdk.ToolResult, error) {
				scope := extension.ToolScopeFrom(current)
				var checks atomic.Int32
				current = extension.WithToolAccountCheck(current, selection, func(admission context.Context) error {
					if checks.Add(1) != 2 {
						return nil
					}
					reached <- scope
					select {
					case <-proceed:
						return nil
					case <-admission.Done():
						return admission.Err()
					}
				})
				return original(current, spec, name, args)
			}
			result := make(chan error, 1)
			ctx = extension.WithRequestMetadata(ctx, extension.RequestMetadata{Plugin: fixture.scope.Plugin, RequestID: test.Name()})
			requests.Go(func() {
				_, err := fixture.run(ctx, refreshToolArgs)
				result <- err
			})
			waitToolGatewayStack(test, ctx, "tool queued at the stdin writer", "(*workerClient).reserveWrite(")
			resume()
			var scope sdk.Scope
			select {
			case scope = <-reached:
			case err := <-result:
				test.Fatalf("tool finished before final admission: %v", err)
			case <-ctx.Done():
				test.Fatal("tool never reached final admission")
			}
			receipt, err := fixture.manager.Store().OperationContext(ctx, scope.Plugin, scope.Operation)
			if err != nil || receipt.Scope != scope || receipt.State != "dispatched" || fixture.toolCalls.Load() != 0 {
				test.Fatalf("tool did not reach the pre-write fence: %+v, %d calls: %v", receipt, fixture.toolCalls.Load(), err)
			}
			switch change {
			case "account signed out":
				peer := extension.NewManager(fixture.manager.Namespace())
				test.Cleanup(func() { _ = peer.Close(context.Background()) })
				if err := peer.DeleteSubscriptionAccount(ctx, scope, ""); err != nil {
					test.Fatal(err)
				}
			case "login off":
				if err := provider.SetLoginOn(provider.ExtensionProviderID(scope.Plugin, scope.Provider), scope.Account, false); err != nil {
					test.Fatal(err)
				}
			}
			private := filepath.Join(fixture.manager.Namespace(), "private")
			release, held := filelock.Lock(filepath.Join(private, ".lock"), time.Second)
			if !held {
				test.Fatal("could not take the fixture's private Store lock")
			}
			release = sync.OnceFunc(release)
			defer release()
			close(proceed)
			waitToolGatewayStack(test, ctx, "tool waiting for the private Store after policy check", "storeWaitLock(")
			switch change {
			case "same generation credentials":
				path := filepath.Join(private, scope.Plugin, "accounts.json")
				before, err := os.ReadFile(path)
				if err != nil {
					test.Fatal(err)
				}
				after := bytes.Replace(before, []byte("synthetic-original"), []byte("synthetic-replaced"), 1)
				if bytes.Equal(before, after) {
					test.Fatal("fixture credentials were missing")
				}
				writeToolHandoffReplacement(test, path, after)
			case "discovery off":
				current := settings.Load()
				current.NoAccountDiscovery = true
				if err := settings.Save(current); err != nil {
					test.Fatal(err)
				}
			case "same CAS configuration", "reordered configuration":
				replaceToolConfigFileAtSameVersion(test, fixture, change)
			}
			release()
			err = <-result
			wantCalls, wantState := int32(0), "not-started"
			if change == "unchanged" || change == "reordered configuration" {
				wantCalls, wantState = 1, "completed"
				if err != nil {
					test.Errorf("valid tool was refused: %v", err)
				}
			} else {
				var outcome *extension.OutcomeError
				if !errors.As(err, &outcome) || outcome.Outcome != wantState {
					test.Errorf("revoked tool: got %v, want %s", err, wantState)
				}
			}
			receipt, err = fixture.manager.Store().OperationContext(ctx, scope.Plugin, scope.Operation)
			if err != nil || receipt.Scope != scope || receipt.State != wantState ||
				fixture.toolCalls.Load() != wantCalls || fixture.refreshes.Load() != 0 {
				test.Fatalf("tool sent %d RPCs and %d refreshes, journaled %q; want %d RPCs, 0 refreshes and %q: %v",
					fixture.toolCalls.Load(), fixture.refreshes.Load(), receipt.State, wantCalls, wantState, err)
			}
		})
	}
}

func waitToolGatewayStack(test *testing.T, ctx context.Context, phase, frame string) {
	test.Helper()
	stacks := make([]byte, 1<<20)
	waitToolRefreshPhase(test, ctx, phase, func() bool {
		for stack := range bytes.SplitSeq(stacks[:runtime.Stack(stacks, true)], []byte("\n\n")) {
			if bytes.Contains(stack, []byte("(*Server).toolsMCPRun(")) && bytes.Contains(stack, []byte(frame)) {
				return true
			}
		}
		return false
	})
}

const toolConfigOriginal = `{"mode":"fast","limit":1}`

func replaceToolConfigAtSameVersion(test *testing.T, fixture *toolRefreshFixture, change string) {
	test.Helper()
	replacement := replaceToolConfigFileAtSameVersion(test, fixture, change)
	config, err := fixture.manager.ModuleConfig(test.Context(), fixture.scope.Plugin)
	if err != nil || config.Version != "1" || string(config.Values) != replacement {
		test.Fatalf("replacement configuration was not read at CAS 1: %+v, %v", config, err)
	}
}

func replaceToolConfigFileAtSameVersion(test *testing.T, fixture *toolRefreshFixture, change string) string {
	test.Helper()
	directory := filepath.Join(fixture.manager.Namespace(), "private", fixture.scope.Plugin, "state")
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		test.Fatalf("want one saved module configuration: %v, %v", entries, err)
	}
	path := filepath.Join(directory, entries[0].Name())
	info, err := os.Stat(path)
	if err != nil {
		test.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		test.Fatal(err)
	}
	replacement := `{"mode":"safe","limit":1}`
	if change == "reordered configuration" {
		replacement = `{"limit":1,"mode":"fast"}`
	}
	body := bytes.Replace(original, []byte(toolConfigOriginal), []byte(replacement), 1)
	if bytes.Equal(body, original) || len(body) != len(original) {
		test.Fatal("configuration replacement did not preserve size")
	}
	writeToolHandoffReplacement(test, path, body)
	replaced, err := os.Stat(path)
	if err != nil || os.SameFile(info, replaced) || replaced.Size() != info.Size() || !replaced.ModTime().Equal(info.ModTime()) {
		test.Fatalf("replacement did not preserve stamp or change file identity: %v, %v", replaced, err)
	}
	return replacement
}

func writeToolHandoffReplacement(test *testing.T, path string, body []byte) {
	test.Helper()
	info, err := os.Stat(path)
	if err != nil {
		test.Fatal(err)
	}
	next := path + ".replacement"
	if err := os.WriteFile(next, body, 0600); err != nil {
		test.Fatal(err)
	}
	if err := os.Chtimes(next, info.ModTime(), info.ModTime()); err != nil {
		test.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		test.Fatal(err)
	}
}
