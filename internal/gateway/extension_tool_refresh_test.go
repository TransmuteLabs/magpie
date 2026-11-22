package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gwtool"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const refreshToolSpec = "extension:tool.refresh"

var refreshToolArgs = json.RawMessage(`{"value":"hello","_magpie":{"provider":"vendor","account":"selected"}}`)

type toolRefreshFixture struct {
	manager   *extension.Manager
	scope     sdk.Scope
	refreshes atomic.Int32
	toolCalls atomic.Int32
	tool      mcpTool
	server    *httptest.Server
}

func installToolRefreshFixture(test *testing.T, beforeReply func(*toolRefreshFixture)) *toolRefreshFixture {
	test.Helper()
	fresh(test)
	test.Setenv("MAGPIE_NO_ACCOUNTS", "")
	fixture := &toolRefreshFixture{manager: extension.Default(), scope: sdk.Scope{Plugin: "tool.refresh", Provider: "vendor", Account: "selected", Generation: 1}}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || (request.URL.Path != "/" && request.URL.Path != "/tool") {
			http.NotFound(writer, request)
			return
		}
		if request.URL.Path == "/tool" {
			fixture.toolCalls.Add(1)
			writer.WriteHeader(http.StatusOK)
			return
		}
		fixture.refreshes.Add(1)
		var call sdk.Call
		if err := json.NewDecoder(request.Body).Decode(&call); err != nil || call.Scope.Generation != 1 || !strings.Contains(string(call.Auth), "synthetic-original") {
			test.Errorf("refresh did not use original Frame.Auth: %+v %v", call.Scope, err)
		}
		if beforeReply != nil {
			beforeReply(fixture)
		}
		auth, _ := json.Marshal(map[string]string{"url": fixture.server.URL, "token": "synthetic-rotated"})
		_ = json.NewEncoder(writer).Encode(sdk.AuthResult{ID: "selected", Credentials: auth, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	}))
	test.Cleanup(fixture.server.Close)
	directory := test.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	build := proc.CommandContext(testenv.DeadlineContext(test, 2*time.Second), filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", filepath.Join(directory, entry), "./testdata/extensiontoolrefreshworker")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		test.Fatalf("build fixture: %v\n%s", err, output)
	}
	manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: fixture.scope.Plugin, Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract:  sdk.Contract{Version: sdk.FullContract().Version, Capabilities: []sdk.Capability{sdk.AccountAuth, sdk.ToolProvider}},
		Providers: []sdk.ProviderDescriptor{{ID: "vendor", Name: "Fixture"}},
		Tools:     []sdk.ToolDescriptor{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.manager.Install(test.Context(), directory); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := fixture.manager.Close(ctx); err != nil {
			test.Error(err)
		}
		provider.Changed()
	})
	auth, _ := json.Marshal(map[string]string{"url": fixture.server.URL, "token": "synthetic-original"})
	if _, err := fixture.manager.Store().SaveAccount(extension.Account{Plugin: fixture.scope.Plugin, Provider: fixture.scope.Provider, ID: fixture.scope.Account,
		Auth: auth, ExpiresAt: time.Now().Add(-time.Minute).UnixMilli()}, 0); err != nil {
		test.Fatal(err)
	}
	if _, err := provider.ExtensionProviders(); err != nil {
		test.Fatal(err)
	}
	groups := []plugin.PluginTools{{Spec: refreshToolSpec, Tools: []plugin.Tool{{Name: "echo"}}}}
	fakePluginTools(test, groups)
	runPluginTool = plugin.RunTool
	fixture.tool = namedTools(groups)[0]
	test.Cleanup(gwtool.Serve(gwtool.PluginTools))
	return fixture
}

func (fixture *toolRefreshFixture) run(ctx context.Context, args json.RawMessage) (map[string]any, error) {
	request := httptest.NewRequest(http.MethodPost, ToolsMCPPath, nil).WithContext(ctx)
	request.RemoteAddr = "127.0.0.1:12345"
	return New().toolsMCPRun(request, fixture.tool, args)
}

func checkRefreshedTool(test *testing.T, output any, err error) {
	test.Helper()
	if err != nil {
		test.Errorf("selected tool failed: %v", err)
		return
	}
	body, direct := output.(string)
	if !direct {
		raw, err := json.Marshal(output)
		if err != nil {
			test.Error(err)
			return
		}
		var result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &result); err != nil || len(result.Content) != 1 {
			test.Errorf("invalid MCP tool result: %s, %v", raw, err)
			return
		}
		body = result.Content[0].Text
	}
	var frame struct {
		Scope sdk.Scope `json:"scope"`
		Auth  struct {
			Token string `json:"token"`
		} `json:"auth"`
		Params sdk.ToolRequest `json:"params"`
	}
	if err := json.Unmarshal([]byte(body), &frame); err != nil {
		test.Errorf("invalid worker frame observation: %s, %v", body, err)
		return
	}
	if frame.Scope.Plugin != "tool.refresh" || frame.Scope.Provider != "vendor" || frame.Scope.Account != "selected" ||
		frame.Scope.Generation != 2 || frame.Auth.Token != "synthetic-rotated" || frame.Params.ID != "echo" ||
		string(frame.Params.Arguments) != `{"value":"hello"}` {
		test.Errorf("tool did not receive refreshed Frame.Auth and sanitized args: %s", body)
	}
}

func TestExtensionToolExpiryPreflightOrdinaryPaths(test *testing.T) {
	for _, route := range []string{"MCP", "Library text", "Library result"} {
		test.Run(route, func(test *testing.T) {
			fixture := installToolRefreshFixture(test, nil)
			var output any
			var err error
			switch route {
			case "MCP":
				output, err = fixture.run(test.Context(), refreshToolArgs)
			case "Library text":
				output, err = RunPluginToolOf(test.Context(), refreshToolSpec, "echo", refreshToolArgs, "fixture")
			case "Library result":
				output, err = RunPluginToolResultOf(test.Context(), refreshToolSpec, "echo", refreshToolArgs, "fixture")
			}
			checkRefreshedTool(test, output, err)
			if fixture.refreshes.Load() != 1 {
				test.Errorf("refresh exchanges = %d; want 1", fixture.refreshes.Load())
			}
		})
	}
}

func TestExtensionToolExpiryPreflightConcurrent(test *testing.T) {
	fixture := installToolRefreshFixture(test, nil)
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() {
			output, err := fixture.run(test.Context(), refreshToolArgs)
			checkRefreshedTool(test, output, err)
		})
	}
	wait.Wait()
	if fixture.refreshes.Load() != 1 {
		test.Fatalf("rotating exchanges = %d; want 1", fixture.refreshes.Load())
	}
}

func TestExtensionToolExpiryPreflightRecheckedAfterWorkerQueue(test *testing.T) {
	for _, change := range []string{"same CAS configuration", "reordered configuration", "login off", "discovery off", "account replaced", "canceled"} {
		test.Run(change, func(test *testing.T) {
			fixture := installToolRefreshFixture(test, nil)
			ctx, cancel := context.WithTimeout(test.Context(), 30*time.Second)
			defer cancel()
			if _, err := fixture.manager.CompareAndSwapModuleConfig(ctx, fixture.scope.Plugin, "0", json.RawMessage(toolConfigOriginal)); err != nil {
				test.Fatal(err)
			}
			release := holdToolRefreshWorkerSlots(test, ctx, fixture)
			caller, stopCaller := context.WithCancel(ctx)
			defer stopCaller()
			request := httptest.NewRequest(http.MethodPost, ToolsMCPPath, strings.NewReader(toolsCall(fixture.tool.name, string(refreshToolArgs)))).WithContext(caller)
			request.RemoteAddr, request.Host = "127.0.0.1:50000", "127.0.0.1:3425"
			request.Header.Set("Content-Type", "application/json")
			handler := lanGuard(New().Handler())
			result := make(chan *httptest.ResponseRecorder, 1)
			var requestDone sync.WaitGroup
			requestDone.Go(func() {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				result <- response
			})
			defer func() {
				stopCaller()
				release()
				requestDone.Wait()
			}()
			waitQueuedToolRefresh(test, ctx, result)

			var err error
			switch change {
			case "same CAS configuration", "reordered configuration":
				replaceToolConfigAtSameVersion(test, fixture, change)
			case "login off":
				err = provider.SetLoginOn(provider.ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider), fixture.scope.Account, false)
			case "discovery off":
				current := settings.Load()
				current.NoAccountDiscovery = true
				err = settings.Save(current)
			case "account replaced":
				_, err = fixture.manager.Store().SaveAccount(extension.Account{
					Plugin: fixture.scope.Plugin, Provider: fixture.scope.Provider, ID: fixture.scope.Account,
					Auth: json.RawMessage(`{"token":"synthetic-manual"}`),
				}, fixture.scope.Generation)
			case "canceled":
				stopCaller()
			}
			if err != nil {
				test.Fatal(err)
			}
			if change != "canceled" {
				release()
			}
			success := change == "reordered configuration"
			select {
			case response := <-result:
				if change != "canceled" && readMCP(test, response).Result.IsError == success {
					test.Errorf("unexpected MCP response: %s", response.Body)
				}
			case <-ctx.Done():
				test.Fatal("queued MCP request did not finish")
			}
			if change == "canceled" {
				release()
				waitToolRefreshOwner(test, ctx)
			}
			wantCalls, wantGeneration := int32(0), fixture.scope.Generation
			if success {
				wantCalls, wantGeneration = 1, wantGeneration+1
			}
			if change == "account replaced" {
				wantGeneration++
			}
			account, err := fixture.manager.Store().GetAccountContext(ctx, fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
			if err != nil || account.Generation != wantGeneration ||
				fixture.refreshes.Load() != wantCalls || fixture.toolCalls.Load() != wantCalls {
				test.Errorf("queued selection refreshed %d times and called tool %d times; account generation %d, want %d and %d calls: %v",
					fixture.refreshes.Load(), fixture.toolCalls.Load(), account.Generation, wantGeneration, wantCalls, err)
			}
			if !success {
				assertToolRefreshUnclaimed(test, fixture)
			}
			if change == "canceled" {
				output, err := fixture.run(ctx, refreshToolArgs)
				checkRefreshedTool(test, output, err)
				if fixture.refreshes.Load() != 1 || fixture.toolCalls.Load() != 1 {
					test.Errorf("fresh request after cancellation refreshed %d times and called tool %d times",
						fixture.refreshes.Load(), fixture.toolCalls.Load())
				}
			}
		})
	}
}

func holdToolRefreshWorkerSlots(test *testing.T, ctx context.Context, fixture *toolRefreshFixture) func() {
	test.Helper()
	count := sdk.MaxConcurrent/2 - 2
	occupied := make(chan struct{}, count)
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	blockerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var call sdk.Call
		if request.Method != http.MethodPost || request.URL.Path != "/tool" ||
			json.NewDecoder(request.Body).Decode(&call) != nil || call.Scope.Account != "blocker" {
			http.NotFound(writer, request)
			return
		}
		occupied <- struct{}{}
		select {
		case <-released:
			writer.WriteHeader(http.StatusOK)
		case <-request.Context().Done():
		}
	}))
	auth, _ := json.Marshal(map[string]string{"url": blockerServer.URL})
	blocker, err := fixture.manager.Store().SaveAccount(extension.Account{
		Plugin: fixture.scope.Plugin, Provider: fixture.scope.Provider, ID: "blocker", Auth: auth,
	}, 0)
	var workers sync.WaitGroup
	test.Cleanup(func() {
		release()
		workers.Wait()
		blockerServer.Close()
	})
	if err != nil {
		test.Fatal(err)
	}
	results := make(chan error, count)
	for index := range count {
		workers.Go(func() {
			_, err := fixture.manager.Call(ctx, blocker.Plugin, sdk.MethodToolCall, sdk.Scope{
				Provider: blocker.Provider, Account: blocker.ID, Generation: blocker.Generation,
				Operation: "hold-refresh-" + strconv.Itoa(index), Attempt: 1,
			}, sdk.ToolRequest{ID: "echo", Arguments: json.RawMessage(`{}`)}, nil)
			results <- err
		})
	}
	for range count {
		select {
		case <-occupied:
		case err := <-results:
			test.Fatalf("tool slot holder returned before release: %v", err)
		case <-ctx.Done():
			test.Fatal("tool worker slots did not fill")
		}
	}
	return release
}

func waitQueuedToolRefresh(test *testing.T, ctx context.Context, result <-chan *httptest.ResponseRecorder) {
	test.Helper()
	stacks := make([]byte, 1<<20)
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		if bytes.Contains(toolRefreshOwnerStack(stacks), []byte("(*workerClient).callWithAdmission(")) {
			return
		}
		select {
		case response := <-result:
			test.Fatalf("MCP returned before refresh reached the worker queue: %s", response.Body)
		case <-ctx.Done():
			test.Fatal("refresh never reached the full worker queue")
		case <-poll.C:
		}
	}
}

func waitToolRefreshOwner(test *testing.T, ctx context.Context) {
	test.Helper()
	stacks := make([]byte, 1<<20)
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for toolRefreshOwnerStack(stacks) != nil {
		select {
		case <-poll.C:
		case <-ctx.Done():
			test.Fatal("canceled refresh owner did not finish")
		}
	}
}

func TestExtensionToolExpiryPreflightRecheckedAfterStdinDrains(test *testing.T) {
	for _, change := range []string{"unchanged", "login off", "discovery off", "same CAS configuration", "account replaced", "canceled"} {
		test.Run(change, func(test *testing.T) {
			fixture := installToolRefreshFixture(test, nil)
			ctx, cancel := context.WithTimeout(test.Context(), 20*time.Second)
			defer cancel()
			if _, err := fixture.manager.CompareAndSwapModuleConfig(ctx, fixture.scope.Plugin, "0", json.RawMessage(toolConfigOriginal)); err != nil {
				test.Fatal(err)
			}
			directory := test.TempDir()
			resume := sync.OnceValue(func() error { return os.WriteFile(filepath.Join(directory, "continue"), nil, 0600) })
			var running sync.WaitGroup
			defer func() {
				if err := resume(); err != nil {
					test.Error(err)
				}
				cancel()
				running.Wait()
			}()
			marker, err := json.Marshal(map[string]string{"fixture-stop-reading": directory})
			if err != nil {
				test.Fatal(err)
			}
			callPublic := func(arguments json.RawMessage) <-chan error {
				returned := make(chan error, 1)
				running.Go(func() {
					_, err := fixture.manager.Call(ctx, fixture.scope.Plugin, sdk.MethodToolCall, sdk.Scope{},
						sdk.ToolRequest{ID: "echo", Arguments: arguments}, nil)
					returned <- err
				})
				return returned
			}
			markerDone := callPublic(marker)
			waitToolRefreshPhase(test, ctx, "worker stdin pause", func() bool {
				_, err := os.Stat(filepath.Join(directory, "ready"))
				return err == nil
			})
			padding := json.RawMessage(`{"fixture-padding":"` + strings.Repeat("x", 2<<20) + `"}`)
			paddedDone := callPublic(padding)
			stacks := make([]byte, 1<<20)
			waitToolRefreshPhase(test, ctx, "worker stuck in stdin write", func() bool {
				for stack := range bytes.SplitSeq(stacks[:runtime.Stack(stacks, true)], []byte("\n\n")) {
					if bytes.Contains(stack, []byte("(*workerClient).writeLoop(")) &&
						bytes.Contains(stack, []byte("os.(*File).Write(")) {
						return true
					}
				}
				return false
			})
			type response struct {
				output any
				err    error
			}
			reply := make(chan response, 1)
			caller, stopCaller := context.WithCancel(ctx)
			defer stopCaller()
			running.Go(func() {
				output, err := fixture.run(caller, refreshToolArgs)
				reply <- response{output, err}
			})
			waitToolRefreshPhase(test, ctx, "claimed refresh queued behind stdin", func() bool {
				scope, state := toolRefreshReceipt(test, fixture)
				if scope.Operation == "" || state != "dispatched" {
					return false
				}
				stack := toolRefreshOwnerStack(stacks)
				return bytes.Contains(stack, []byte("(*workerClient).callWithAdmission(")) &&
					(bytes.Contains(stack, []byte("(*workerClient).send")) || bytes.Contains(stack, []byte("(*workerClient).reserveWrite(")))
			})
			claimed, _ := toolRefreshReceipt(test, fixture)

			switch change {
			case "login off":
				err = provider.SetLoginOn(provider.ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider), fixture.scope.Account, false)
			case "discovery off":
				current := settings.Load()
				current.NoAccountDiscovery = true
				err = settings.Save(current)
			case "same CAS configuration":
				replaceToolConfigAtSameVersion(test, fixture, change)
			case "account replaced":
				_, err = fixture.manager.Store().SaveAccount(extension.Account{
					Plugin: fixture.scope.Plugin, Provider: fixture.scope.Provider, ID: fixture.scope.Account,
					Auth: json.RawMessage(`{"token":"synthetic-manual"}`),
				}, fixture.scope.Generation)
			case "canceled":
				stopCaller()
			}
			if err != nil {
				test.Fatal(err)
			}
			if err := resume(); err != nil {
				test.Fatal(err)
			}
			for _, done := range []<-chan error{markerDone, paddedDone} {
				select {
				case err := <-done:
					if err != nil {
						test.Fatal(err)
					}
				case <-ctx.Done():
					test.Fatal("ordinary frames did not drain")
				}
			}
			select {
			case result := <-reply:
				if change == "unchanged" {
					checkRefreshedTool(test, result.output, result.err)
				} else if result.err == nil {
					test.Errorf("revoked MCP request succeeded: %v", result.output)
				}
			case <-ctx.Done():
				test.Fatal("MCP did not finish after worker stdin drained")
			}
			waitToolRefreshOwner(test, ctx)
			want, wantState := int32(0), "not-started"
			if change == "unchanged" {
				want, wantState = 1, "completed"
			}
			got, state := toolRefreshReceipt(test, fixture)
			if got != claimed || state != wantState || fixture.refreshes.Load() != want || fixture.toolCalls.Load() != want {
				test.Errorf("refresh=%d tool=%d receipt=%+v state=%q; want %d, same receipt %q in %q",
					fixture.refreshes.Load(), fixture.toolCalls.Load(), got, state, want, claimed.Operation, wantState)
			}
		})
	}
}

func waitToolRefreshPhase(test *testing.T, ctx context.Context, phase string, check func() bool) {
	test.Helper()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for !check() {
		select {
		case <-ctx.Done():
			test.Fatalf("did not reach %s: %v", phase, ctx.Err())
		case <-poll.C:
		}
	}
}

func toolRefreshOwnerStack(stacks []byte) []byte {
	for stack := range bytes.SplitSeq(stacks[:runtime.Stack(stacks, true)], []byte("\n\n")) {
		if bytes.Contains(stack, []byte("(*Manager).ownRefresh(")) {
			return stack
		}
	}
	return nil
}

func assertToolRefreshUnclaimed(test *testing.T, fixture *toolRefreshFixture) {
	test.Helper()
	if scope, _ := toolRefreshReceipt(test, fixture); scope.Operation != "" {
		test.Error("queued, revoked refresh wrote a dispatch receipt")
	}
}

func toolRefreshReceipt(test *testing.T, fixture *toolRefreshFixture) (sdk.Scope, string) {
	test.Helper()
	path := filepath.Join(fixture.manager.Namespace(), "private", fixture.scope.Plugin, "accounts.json")
	raw, err := os.ReadFile(path)
	var document struct {
		Accounts []struct {
			ID      string `json:"id"`
			Refresh struct {
				Scope sdk.Scope `json:"scope"`
				State string    `json:"state"`
			} `json:"refresh"`
		} `json:"accounts"`
	}
	if err != nil || json.Unmarshal(raw, &document) != nil {
		test.Fatalf("read refresh receipts: %v", err)
	}
	for _, account := range document.Accounts {
		if account.ID == fixture.scope.Account {
			return account.Refresh.Scope, account.Refresh.State
		}
	}
	test.Fatal("selected account is missing from fixture Store")
	return sdk.Scope{}, ""
}

func TestExtensionToolExpiryPreflightControls(test *testing.T) {
	for _, mode := range []string{"unbound", "unknown expiry", "fresh", "network hold", "account off", "disabled", "restricted caller", "invalid args", "canceled", "fresh request error"} {
		test.Run(mode, func(test *testing.T) {
			fixture := installToolRefreshFixture(test, nil)
			ctx, args := test.Context(), refreshToolArgs
			rejected := false
			switch mode {
			case "unbound":
				args = json.RawMessage(`{"value":"public"}`)
			case "unknown expiry", "fresh", "fresh request error":
				account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
				if err != nil {
					test.Fatal(err)
				}
				account.ExpiresAt = 0
				if mode != "unknown expiry" {
					account.ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
				}
				if _, err := fixture.manager.Store().SaveAccount(account, account.Generation); err != nil {
					test.Fatal(err)
				}
				if mode == "fresh request error" {
					args = json.RawMessage(`{"value":"fixture-401","_magpie":{"provider":"vendor","account":"selected"}}`)
					rejected = true
				}
			case "restricted caller":
				ctx, rejected = access.WithIdentity(ctx, access.Identity{ReadOnly: true}), true
			case "invalid args":
				args, rejected = json.RawMessage(`[]`), true
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				rejected = true
			case "network hold":
				ctx, rejected = netproxy.Hold(ctx, errors.New("fixture hold")), true
			case "account off":
				if err := provider.SetLoginOn(provider.ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider), fixture.scope.Account, false); err != nil {
					test.Fatal(err)
				}
				rejected = true
			case "disabled":
				if err := fixture.manager.SetEnabled(ctx, fixture.scope.Plugin, false); err != nil {
					test.Fatal(err)
				}
				rejected = true
			}
			output, err := fixture.run(ctx, args)
			if rejected && mode != "fresh request error" && fixture.toolCalls.Load() != 0 {
				test.Error("rejected control reached tool callback")
			}
			if mode == "fresh request error" && fixture.toolCalls.Load() != 1 {
				test.Error("request error was replayed or never dispatched")
			}
			if (err != nil) != rejected {
				test.Errorf("control output=%v error=%v rejected=%v", output, err, rejected)
			}
			if fixture.refreshes.Load() != 0 {
				test.Errorf("control refreshed: %d", fixture.refreshes.Load())
			}
		})
	}
}

func TestExtensionToolExpiryPreflightRevocationDuringRefresh(test *testing.T) {
	for _, mode := range []string{"manual replacement", "config", "account off", "disabled"} {
		test.Run(mode, func(test *testing.T) {
			fixture := installToolRefreshFixture(test, func(fixture *toolRefreshFixture) {
				switch mode {
				case "manual replacement":
					_, err := fixture.manager.Store().SaveAccount(extension.Account{Plugin: fixture.scope.Plugin, Provider: fixture.scope.Provider, ID: fixture.scope.Account,
						Auth: json.RawMessage(`{"token":"manual"}`), ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, 1)
					if err != nil {
						test.Error(err)
					}
				case "config":
					if _, err := fixture.manager.CompareAndSwapModuleConfig(test.Context(), fixture.scope.Plugin, "0", json.RawMessage(`{"changed":true}`)); err != nil {
						test.Error(err)
					}
				case "disabled":
					if err := extension.NewCatalog(fixture.manager.Namespace()).SetEnabled(test.Context(), fixture.scope.Plugin, false); err != nil {
						test.Error(err)
					}
				case "account off":
					if err := provider.SetLoginOn(provider.ExtensionProviderID(fixture.scope.Plugin, fixture.scope.Provider), fixture.scope.Account, false); err != nil {
						test.Error(err)
					}
				}
			})
			output, err := fixture.run(test.Context(), refreshToolArgs)
			if err == nil {
				test.Errorf("revoked selection dispatched tool: %v", output)
			}
			if fixture.refreshes.Load() != 1 {
				test.Errorf("refresh exchange count = %d", fixture.refreshes.Load())
			}
			if fixture.toolCalls.Load() != 0 {
				test.Errorf("revoked selection reached tool callback")
			}
			if mode == "manual replacement" {
				account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
				if err != nil || account.Generation != 2 || string(account.Auth) != `{"token":"manual"}` {
					test.Errorf("refresh overwrote manual replacement: generation=%d, error=%v", account.Generation, err)
				}
			}
			if mode == "disabled" {
				if _, err := fixture.manager.Manifest(fixture.scope.Plugin); err == nil {
					test.Error("refresh re-enabled the disabled module")
				}
			}
			if mode == "account off" {
				before := fixture.refreshes.Load()
				if _, err := fixture.run(test.Context(), refreshToolArgs); err == nil {
					test.Error("refresh re-enabled the disabled account")
				}
				if fixture.refreshes.Load() != before {
					test.Error("disabled account triggered another refresh")
				}
			}

		})
	}
}

func TestExtensionToolExpiryPreflightRevocationBeforeDispatch(test *testing.T) {
	for _, mode := range []string{"manual replacement", "config", "disabled", "disabled and enabled", "hold"} {
		test.Run(mode, func(test *testing.T) {
			fixture := installToolRefreshFixture(test, nil)
			ctx, args, err := extensionToolContext(test.Context(), refreshToolSpec, refreshToolArgs)
			if err != nil {
				test.Fatal(err)
			}
			switch mode {
			case "manual replacement":
				account, err := fixture.manager.Store().GetAccount(fixture.scope.Plugin, fixture.scope.Provider, fixture.scope.Account)
				if err != nil {
					test.Fatal(err)
				}
				account.Auth = json.RawMessage(`{"token":"manual"}`)
				if _, err := fixture.manager.Store().SaveAccount(account, account.Generation); err != nil {
					test.Fatal(err)
				}
			case "config":
				if _, err := fixture.manager.CompareAndSwapModuleConfig(ctx, fixture.scope.Plugin, "0", json.RawMessage(`{"changed":true}`)); err != nil {
					test.Fatal(err)
				}
			case "disabled":
				if err := fixture.manager.SetEnabled(ctx, fixture.scope.Plugin, false); err != nil {
					test.Fatal(err)
				}
			case "disabled and enabled":
				catalog := extension.NewCatalog(fixture.manager.Namespace())
				if err := catalog.SetEnabled(ctx, fixture.scope.Plugin, false); err != nil {
					test.Fatal(err)
				}
				if err := catalog.SetEnabled(ctx, fixture.scope.Plugin, true); err != nil {
					test.Fatal(err)
				}
			case "hold":
				ctx = netproxy.Hold(ctx, errors.New("fixture late hold"))
			}
			if output, err := plugin.RunExtensionTool(ctx, refreshToolSpec, "echo", args); err == nil {
				test.Errorf("late revoked tool dispatched: %s", output.Content)
			}
			if fixture.toolCalls.Load() != 0 {
				test.Error("late revoked selection reached tool callback")
			}
		})
	}
}
