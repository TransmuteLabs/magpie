package extension

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
	codec "github.com/yetone/magpie/pkg/protocolext"
)

func wedgedInputHelper() {
	body, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		os.Exit(1)
	}
	var manifest sdk.Manifest
	if json.Unmarshal(body, &manifest) != nil {
		os.Exit(1)
	}
	input := bufio.NewScanner(os.Stdin)
	input.Buffer(make([]byte, 4096), sdk.MaxFrameBytes+1)
	output := json.NewEncoder(os.Stdout)
	var ready *sdk.ComponentReadyResult
	for input.Scan() {
		var frame sdk.Frame
		if json.Unmarshal(input.Bytes(), &frame) != nil {
			os.Exit(1)
		}
		var result any
		switch frame.Method {
		case sdk.MethodDescribe:
			result = manifest.Public()
		case sdk.MethodComponentConfigure:
			var configuration sdk.ComponentConfigureRequest
			if json.Unmarshal(frame.Params, &configuration) != nil {
				os.Exit(1)
			}
			ready = &sdk.ComponentReadyResult{
				Component: configuration.Component, Contract: configuration.Contract, Instance: configuration.Instance,
				ConfigVersion: configuration.Config.Version, Endpoints: map[string]string{},
			}
			result = ready
		case sdk.MethodComponentReady:
			if ready == nil {
				os.Exit(1)
			}
			result = ready
		case sdk.MethodComponentStop:
			ready = nil
			result = struct{}{}
		default:
			if frame.Method != sdk.MethodToolCall || manifest.Contract.Version.Minor >= 1 && ready == nil {
				os.Exit(1)
			}
			// Complete control RPCs before simulating a payload handler that
			// stops reading stdin, including its ACK and cancellation frames.
			body, _ := json.Marshal(ready)
			if output.Encode(sdk.Frame{Kind: "event", ID: frame.ID, Seq: 1, Event: "ready", Data: body}) != nil {
				return
			}
			for {
				time.Sleep(time.Hour)
			}
		}
		body, err := json.Marshal(result)
		if err != nil || output.Encode(sdk.Frame{Kind: "result", ID: frame.ID, Result: body}) != nil {
			return
		}
	}
}

func helperModule() sdk.Module {
	var manifest sdk.Manifest
	if b, err := os.ReadFile(sdk.ManifestFile); err != nil || json.Unmarshal(b, &manifest) != nil {
		panic("missing fixture manifest")
	}
	handlers := map[string]sdk.Handler{
		sdk.MethodProviderExecute: func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
			var request sdk.ExecuteRequest
			if json.Unmarshal(call.Params, &request) != nil {
				return nil, errors.New("bad fixture request")
			}
			if request.Model == "reject" {
				return nil, &sdk.Error{Code: "unavailable", Message: "fixture refused before execution", Outcome: "not-started"}
			}
			if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
				return nil, err
			}
			if request.Model == "crash" {
				os.Exit(23)
			}
			if err := stream.Emit(ctx, "headers", sdk.HeadersEvent{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}}); err != nil {
				return nil, err
			}
			answer := []byte(`{"id":"fixture","choices":[{"message":{"role":"assistant","content":"hello from worker"},"finish_reason":"stop"}]}`)
			if err := stream.Emit(ctx, "data", answer); err != nil {
				return nil, err
			}
			return sdk.ExecuteResult{Outcome: "completed", Usage: &sdk.Usage{InputTokens: 3, OutputTokens: 4}}, nil
		},
		sdk.MethodProviderModels: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
			return sdk.ModelsResult{Models: []sdk.Model{{ID: "model", Name: "Fixture", ContextWindow: 65536}}}, nil
		},
		sdk.MethodProviderUsage: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
			return sdk.UsageResult{Windows: []sdk.UsageWindow{{Name: "daily", Used: 12}}}, nil
		},
		sdk.MethodAuthStart: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
			return sdk.AuthStartResult{UserCode: "fixture-code", Continuation: json.RawMessage(`{"nonce":"fixture"}`)}, nil
		},
		sdk.MethodAuthFinish: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
			return sdk.AuthResult{ID: "account", Credentials: json.RawMessage(`{"token":"only-this-account"}`)}, nil
		},
		sdk.MethodAuthRefresh: func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			return sdk.AuthResult{ID: call.Scope.Account, Credentials: call.Auth}, nil
		},
		sdk.MethodAuthLogout: func(context.Context, sdk.Call, *sdk.Stream) (any, error) {
			return map[string]bool{"ok": true}, nil
		},
		sdk.MethodOperationStatus: func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			return sdk.OperationResult{OperationID: call.Scope.Operation, Outcome: "completed"}, nil
		},
		sdk.MethodOperationCancel: func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			return sdk.OperationResult{OperationID: call.Scope.Operation, Outcome: "canceled"}, nil
		},
		sdk.MethodToolCall: fixtureTool,
		sdk.MethodAgentPlan: func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			return sdk.AgentPlan{Files: []sdk.FileChange{{Path: "config.json", Body: []byte(`{"model":"fixture"}`)}}}, nil
		},
		sdk.MethodSessionConvert: func(_ context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
			var request sdk.SessionRequest
			if err := json.Unmarshal(call.Params, &request); err != nil {
				return nil, err
			}
			for index := range request.Files {
				request.Files[index].Format = request.To
				request.Files[index].Body = bytes.ToUpper(request.Files[index].Body)
			}
			return sdk.SessionResult{Files: request.Files}, nil
		},
	}
	required := sdk.RequiredMethods(manifest)
	for name := range handlers {
		if !slices.Contains(required, name) {
			delete(handlers, name)
		}
	}
	var inbounds []codec.Inbound
	if manifest.Contract.Has(sdk.InboundCodec) {
		inbounds = []codec.Inbound{fixtureCodec{}}
	}
	return sdk.Module{Manifest: manifest, Handlers: handlers, Inbounds: inbounds}
}

func fixtureTool(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var request sdk.ToolRequest
	if err := json.Unmarshal(call.Params, &request); err != nil {
		return nil, err
	}
	var args struct {
		Action  string          `json:"action"`
		Key     string          `json:"key"`
		Version string          `json:"version"`
		Value   json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(request.Arguments, &args); err != nil {
		return nil, err
	}
	switch args.Action {
	case "stream":
		for _, text := range []string{"first", "second"} {
			if err := stream.Emit(ctx, "data", []byte(text)); err != nil {
				return nil, err
			}
		}
	case "wait":
		if err := stream.Emit(ctx, "ready", nil); err != nil {
			return nil, err
		}
		<-ctx.Done()
		return nil, ctx.Err()
	case "get":
		var value sdk.StateValue
		if err := stream.HostCall(ctx, sdk.MethodStateGet, sdk.StateGetRequest{Key: args.Key}, &value); err != nil {
			return nil, err
		}
		return value, nil
	case "cas":
		var value sdk.StateCASResult
		if err := stream.HostCall(ctx, sdk.MethodStateCAS, sdk.StateCASRequest{Key: args.Key, Version: args.Version, Value: args.Value}, &value); err != nil {
			return nil, err
		}
		return value, nil
	case "auth":
		return json.RawMessage(call.Auth), nil
	case "proxy":
		var value sdk.NetworkProxyResult
		if err := stream.HostCall(ctx, sdk.MethodNetworkProxy, sdk.NetworkProxyRequest{URL: args.Key}, &value); err != nil {
			return nil, err
		}
		return value, nil
	case "replace", "replace-unaccepted":
		if args.Action == "replace" {
			if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
				return nil, err
			}
		}
		var value sdk.AccountReplaceResult
		if err := stream.HostCall(ctx, sdk.MethodAccountReplace, sdk.AccountReplaceRequest{Credentials: args.Value}, &value); err != nil {
			return nil, err
		}
		return value, nil
	}
	return map[string]any{"pid": os.Getpid(), "scope": call.Scope}, nil
}

func TestWorkerHostServicesHonorProxyAndCredentialEpoch(t *testing.T) {
	manager, _ := fixtureManager(t)
	ctx := netproxy.With(context.Background(), "socks5://127.0.0.1:11111")
	result, err := manager.Call(ctx, "fixture", sdk.MethodToolCall, sdk.Scope{},
		sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{"action":"proxy","key":"https://203.0.113.40/endpoint"}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var proxy sdk.NetworkProxyResult
	if json.Unmarshal(result, &proxy) != nil || proxy.URL != "socks5://127.0.0.1:11111" {
		t.Fatalf("scoped proxy: %s", result)
	}
	account, err := manager.Store().SaveAccount(Account{Plugin: "fixture", Provider: "provider", ID: "account", Auth: json.RawMessage(`{"token":"old"}`)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Provider: "provider", Account: account.ID, Generation: account.Generation}
	_, err = manager.Call(context.Background(), "fixture", sdk.MethodToolCall, scope,
		sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{"action":"replace-unaccepted","value":{"token":"must-not-save"}}`)}, nil)
	if err == nil {
		t.Fatal("unaccepted account replacement was granted")
	}
	current, err := manager.Store().GetAccount("fixture", "provider", "account")
	if err != nil || string(current.Auth) != `{"token":"old"}` {
		t.Fatalf("unaccepted replacement changed state: %+v %v", current, err)
	}
	result, err = manager.Call(context.Background(), "fixture", sdk.MethodToolCall, scope,
		sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{"action":"replace","value":{"token":"new"}}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var updated sdk.AccountReplaceResult
	if json.Unmarshal(result, &updated) != nil || updated.ID != "account" || updated.Generation != account.Generation+1 {
		t.Fatalf("replacement result: %s", result)
	}
	current, err = manager.Store().GetAccount("fixture", "provider", "account")
	if err != nil || current.Generation != updated.Generation || string(current.Auth) != `{"token":"new"}` {
		t.Fatalf("replacement was not durable: %+v %v", current, err)
	}
}

func TestWorkerHostStateRoundTripsBeyondStreamChunkSize(t *testing.T) {
	manager, _ := fixtureManager(t)
	value := strings.Repeat("state", 64<<10)
	arguments, _ := json.Marshal(map[string]any{"action": "cas", "key": "large", "value": value})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := manager.Call(ctx, "fixture", sdk.MethodToolCall, sdk.Scope{},
		sdk.ToolRequest{ID: "tool", Arguments: arguments}, nil); err != nil {
		t.Fatal(err)
	}
	var result sdk.StateValue
	if err := manager.Invoke(ctx, "fixture", sdk.MethodToolCall, sdk.Scope{},
		sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{"action":"get","key":"large"}`)}, &result, nil); err != nil {
		t.Fatal(err)
	}
	var decoded string
	if !result.Found || json.Unmarshal(result.Value, &decoded) != nil || decoded != value {
		t.Fatalf("large scoped state: found=%v bytes=%d", result.Found, len(result.Value))
	}
}

type fixtureCodec struct{}

func (fixtureCodec) ID() string               { return "echo" }
func (fixtureCodec) Path() string             { return "/sdk-fixture/echo" }
func (fixtureCodec) Contract() codec.Contract { return codec.CatalogContract() }
func (fixtureCodec) Decode(_ context.Context, headers http.Header, body []byte) (codec.Request, error) {
	var request struct {
		Model   string `json:"model"`
		Prompt  string `json:"prompt"`
		Catalog bool   `json:"catalog"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return codec.Request{}, err
	}
	if headers.Get("Authorization") != "" {
		return codec.Request{}, errors.New("host authority reached the fixture")
	}
	if request.Catalog {
		return codec.Request{Protocol: codec.Models}, nil
	}
	canonical, _ := json.Marshal(map[string]any{"model": request.Model, "messages": []map[string]string{{"role": "user", "content": request.Prompt}}})
	return codec.Request{Protocol: codec.Chat, Body: canonical}, nil
}
func (fixtureCodec) Reply(writer http.ResponseWriter) codec.Response { return fixtureReply{writer} }

type fixtureReply struct{ http.ResponseWriter }

func (fixtureReply) Finish(error) error { return nil }

func fixturePackage(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	entry := "worker"
	if filepath.Ext(os.Args[0]) == ".exe" {
		entry += ".exe"
	}
	input, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(filepath.Join(directory, entry), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(output, input); err != nil {
		t.Fatal(err)
	}
	if err = output.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{
		Schema: 1, ID: "fixture", Version: "1.0.0", Runtime: "exec", Entry: entry, Args: []string{"--sdk-test-worker"},
		Contract:  sdk.FullContract(),
		Inbounds:  []sdk.InboundDescriptor{{ID: "echo", Path: "/sdk-fixture/echo"}},
		Providers: []sdk.ProviderDescriptor{{ID: "provider", Name: "Fixture", Protocols: []codec.Protocol{codec.Chat}, Models: []sdk.Model{{ID: "model"}}}},
		Agents:    []sdk.AgentDescriptor{{ID: "agent", Name: "Fixture agent"}},
		Sessions:  []sdk.SessionDescriptor{{ID: "session", Name: "Fixture session"}},
		Tools:     []sdk.ToolDescriptor{{Name: "tool", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, sdk.ManifestFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return directory
}

func fixtureManager(t *testing.T) (*Manager, Installed) {
	t.Helper()
	manager := NewManager(t.TempDir())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	entry, err := manager.Install(ctx, fixturePackage(t))
	if err != nil {
		t.Fatal(err)
	}
	return manager, entry
}

func toolCall(t *testing.T, manager *Manager, scope sdk.Scope, action string, event func(Event) error) json.RawMessage {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"action": action})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := manager.Call(ctx, "fixture", sdk.MethodToolCall, scope, sdk.ToolRequest{ID: "tool", Arguments: b}, event)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWorkerInstalledAndMultiplexed(t *testing.T) {
	manager, _ := fixtureManager(t)
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := manager.Call(ctx, "fixture", sdk.MethodToolCall, sdk.Scope{}, sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{"action":"stream"}`)}, func(event Event) error {
			if event.Name == "data" {
				select {
				case <-started:
				default:
					close(started)
					<-release
				}
			}
			return nil
		})
		done <- err
	}()
	<-started
	ping := toolCall(t, manager, sdk.Scope{}, "pid", nil)
	if !bytes.Contains(ping, []byte(`"pid"`)) {
		t.Fatal(string(ping))
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWorkerCancellationKeepsNextIndependentCall(t *testing.T) {
	manager, _ := fixtureManager(t)
	first := toolCall(t, manager, sdk.Scope{}, "pid", nil)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := manager.Call(ctx, "fixture", sdk.MethodToolCall, sdk.Scope{}, sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{"action":"wait"}`)}, func(event Event) error {
			if event.Name == "ready" {
				close(started)
			}
			return nil
		})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel result: %v", err)
	}
	second := toolCall(t, manager, sdk.Scope{}, "pid", nil)
	var a, b struct {
		PID int `json:"pid"`
	}
	json.Unmarshal(first, &a)
	json.Unmarshal(second, &b)
	if a.PID != b.PID {
		t.Fatalf("cooperative cancellation restarted worker: %d -> %d", a.PID, b.PID)
	}
}

func TestWorkerAcceptanceDurableAndCrashNeverReplayed(t *testing.T) {
	manager, _ := fixtureManager(t)
	account, err := manager.Store().SaveAccount(Account{Plugin: "fixture", Provider: "provider", ID: "account", Auth: json.RawMessage(`{"token":"selected"}`)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Provider: "provider", Account: account.ID, Generation: account.Generation, Operation: "crash-operation", Attempt: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	accepted := false
	_, err = manager.Call(ctx, "fixture", sdk.MethodProviderExecute, scope, sdk.ExecuteRequest{Protocol: "chat", Model: "crash"}, func(event Event) error {
		if event.Name == "accepted" {
			record, err := manager.Store().Operation("fixture", scope.Operation)
			if err != nil || record.State != "accepted" {
				return fmt.Errorf("not durable before acknowledgement: %+v %v", record, err)
			}
			accepted = true
		}
		return nil
	})
	var outcome *OutcomeError
	if !accepted || !errors.As(err, &outcome) || outcome.Outcome != "uncertain" {
		t.Fatalf("crash: accepted=%v err=%v", accepted, err)
	}
	record, err := manager.Store().Operation("fixture", scope.Operation)
	if err != nil || record.State != "uncertain" {
		t.Fatalf("journal after crash: %+v %v", record, err)
	}
	_, err = manager.Call(ctx, "fixture", sdk.MethodProviderExecute, scope, sdk.ExecuteRequest{Protocol: "chat", Model: "model"}, nil)
	if err == nil {
		t.Fatal("a previous operation was replayed")
	}
	toolCall(t, manager, sdk.Scope{}, "pid", nil)
}

func TestWorkerScopedHostStateAndGeneration(t *testing.T) {
	manager, _ := fixtureManager(t)
	account, err := manager.Store().SaveAccount(Account{Plugin: "fixture", Provider: "provider", ID: "account", Auth: json.RawMessage(`{"secret":"only-selected"}`)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Provider: "provider", Account: account.ID, Generation: account.Generation}
	out, err := manager.Call(context.Background(), "fixture", sdk.MethodToolCall, scope, sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{"action":"cas","key":"value","value":{"x":1}}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var swapped sdk.StateCASResult
	if json.Unmarshal(out, &swapped) != nil || !swapped.Swapped || swapped.Version != "1" {
		t.Fatalf("CAS: %s", out)
	}
	global := toolCall(t, manager, sdk.Scope{}, "auth", nil)
	if string(global) != "null" {
		t.Fatalf("unselected account exposed: %s", global)
	}
	auth := toolCall(t, manager, scope, "auth", nil)
	if string(auth) != `{"secret":"only-selected"}` {
		t.Fatalf("selected auth: %s", auth)
	}
	if _, err = manager.Store().SaveAccount(Account{Plugin: "fixture", Provider: "provider", ID: "account", Auth: json.RawMessage(`{"secret":"rotated"}`)}, account.Generation); err != nil {
		t.Fatal(err)
	}
	_, err = manager.Call(context.Background(), "fixture", sdk.MethodToolCall, scope, sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{}`)}, nil)
	if err == nil {
		t.Fatal("revoked account generation was accepted")
	}
}

func TestWorkerLeaseSurvivesUpgradeAndBlocksDisabledCalls(t *testing.T) {
	manager, entry := fixtureManager(t)
	lease, err := manager.Acquire(context.Background(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err = manager.SetEnabled(context.Background(), "fixture", false); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Call(context.Background(), "fixture", sdk.MethodToolCall, sdk.Scope{}, sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{}`)}, nil); err == nil {
		t.Fatal("disabled module accepted new work")
	}
	result, err := lease.Call(context.Background(), sdk.MethodToolCall, sdk.Scope{}, sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{}`)}, nil)
	if err != nil || !bytes.Contains(result, []byte(`"pid"`)) {
		t.Fatalf("leased generation failed: %s %v", result, err)
	}
	if lease.Revision() != entry.Revision {
		t.Fatal("lease changed generation")
	}
}

func TestWorkerConcurrentCallsHaveDistinctOperations(t *testing.T) {
	manager, _ := fixtureManager(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := manager.Call(context.Background(), "fixture", sdk.MethodToolCall, sdk.Scope{}, sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{}`)}, nil)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkerReservesResponseAndControlCredit(t *testing.T) {
	manager, _ := fixtureManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	registry, err := manager.Protocols()
	if err != nil {
		t.Fatal(err)
	}
	inbound, _ := registry.Lookup("/sdk-fixture/echo")
	inbound = codec.Bind(inbound, ctx)
	recorder := httptest.NewRecorder()
	reply := inbound.Reply(recorder)
	defer reply.Finish(nil)
	if _, err := inbound.Decode(ctx, http.Header{}, []byte(`{"model":"fixture/model","prompt":"test"}`)); err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Acquire(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	count := cap(lease.client.regularSlots)
	waiting := make(chan struct{}, count)
	done := make(chan error, count)
	waits, cancelWaits := context.WithCancel(ctx)
	defer cancelWaits()
	for range count {
		go func() {
			_, err := manager.Call(waits, "fixture", sdk.MethodToolCall, sdk.Scope{},
				sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{"action":"wait"}`)},
				func(event Event) error {
					if event.Name == "ready" {
						waiting <- struct{}{}
					}
					return nil
				})
			done <- err
		}()
	}
	for range count {
		select {
		case <-waiting:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	reply.WriteHeader(200)
	if _, err := reply.Write([]byte("still moving")); err != nil {
		t.Fatal("native response was blocked behind inference:", err)
	}
	var control sdk.OperationResult
	if err := manager.Invoke(ctx, "fixture", sdk.MethodOperationStatus, sdk.Scope{}, sdk.OperationRequest{OperationID: "inspect"}, &control, nil); err != nil {
		t.Fatal("operation control was blocked behind inference:", err)
	}
	cancelWaits()
	for range count {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if recorder.Body.String() != "still moving" {
		t.Fatal(recorder.Body.String())
	}
}

func TestWorkerPackageTamperingIsRefused(t *testing.T) {
	manager, entry := fixtureManager(t)
	if err := os.WriteFile(filepath.Join(manager.root, entry.Directory, sdk.ManifestFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := manager.Call(context.Background(), "fixture", sdk.MethodToolCall, sdk.Scope{}, sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{}`)}, nil)
	if err == nil {
		t.Fatal("tampered package started")
	}
}

func TestWorkerRemovedAuthCapabilityDoesNotReceiveRetainedCredentials(t *testing.T) {
	manager, _ := fixtureManager(t)
	account, err := manager.Store().SaveAccount(Account{Plugin: "fixture", Provider: "provider", ID: "account",
		Auth: json.RawMessage(`{"secret":"retained"}`)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	source := fixturePackage(t)
	path := filepath.Join(source, sdk.ManifestFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Version = "1.0.1"
	manifest.Contract.Capabilities = slices.DeleteFunc(manifest.Contract.Capabilities, func(capability sdk.Capability) bool { return capability == sdk.AccountAuth })
	data, _ = json.Marshal(manifest)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	_, err = manager.Call(context.Background(), "fixture", sdk.MethodToolCall,
		sdk.Scope{Provider: "provider", Account: account.ID, Generation: account.Generation},
		sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{"action":"auth"}`)}, nil)
	if err == nil {
		t.Fatal("a removed account capability still granted credentials")
	}
	if auth := toolCall(t, manager, sdk.Scope{}, "auth", nil); string(auth) != "null" {
		t.Fatalf("credentials leaked to an unbound call: %s", auth)
	}
}

func TestDefaultNamespaceIsStableBeforeConfigExists(t *testing.T) {
	base := t.TempDir()
	actual := filepath.Join(base, "actual")
	if err := os.Mkdir(actual, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(actual, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	missing := filepath.Join(alias, "new", "config")
	before := canonicalConfigPath(missing)
	if err := os.MkdirAll(missing, 0o700); err != nil {
		t.Fatal(err)
	}
	after := canonicalConfigPath(missing)
	if before != after {
		t.Fatalf("creating configuration changed the worker namespace: %q -> %q", before, after)
	}
}

func TestMetadataManagerCanLeaveSynctestBubble(t *testing.T) {
	var manager *Manager
	synctest.Test(t, func(t *testing.T) {
		manager = NewManager(t.TempDir())
		if _, err := manager.List(); err != nil {
			t.Fatal(err)
		}
	})
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestManagerCloseCancelsUnpublishedInstallationWorker(t *testing.T) {
	manager := NewManager(t.TempDir())
	source := fixturePackage(t)
	manifestPath := filepath.Join(source, sdk.ManifestFile)
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err = json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "started")
	manifest.Args = []string{"--sdk-stall-handshake", marker}
	body, _ = json.Marshal(manifest)
	if err = os.WriteFile(manifestPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := manager.Install(ctx, source); done <- err }()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("installation worker did not start:", ctx.Err())
		}
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if err := manager.Close(closeCtx); err != nil {
		t.Fatal("manager shutdown did not own its installation worker:", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("interrupted installation was published")
		}
	case <-ctx.Done():
		t.Fatal("installation outlived manager close")
	}
	entries, err := manager.List()
	if err != nil || len(entries) != 0 {
		t.Fatalf("shutdown published an unfinished installation: %+v %v", entries, err)
	}
}

func TestWorkerCancellationDoesNotWaitForWedgedStdin(t *testing.T) {
	manager := NewManager(t.TempDir())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = manager.Close(ctx)
	})
	source := fixturePackage(t)
	path := filepath.Join(source, sdk.ManifestFile)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err = json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Contract.Version.Major != 1 || manifest.Contract.Version.Minor < 1 {
		t.Fatal("wedged stdin regression must exercise the SDK 1.1 component handshake")
	}
	manifest.Args = []string{"--sdk-wedged-input"}
	body, _ = json.Marshal(manifest)
	if err = os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Install(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Acquire(context.Background(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	ready := make(chan struct{})
	var configuration sdk.ComponentReadyResult
	var configurationErr error
	done := make(chan error, 1)
	callCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, err := manager.Call(callCtx, "fixture", sdk.MethodToolCall, sdk.Scope{},
			sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{}`)}, func(event Event) error {
				if event.Name == "ready" {
					configurationErr = json.Unmarshal(event.Data, &configuration)
					close(ready)
				}
				return nil
			})
		done <- err
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("wedged fixture did not start")
	}
	if configurationErr != nil || configuration.Component != sdk.PrimaryComponent || configuration.Instance == "" ||
		configuration.ConfigVersion != lease.ConfigVersion() {
		t.Fatalf("fixture wedged before completing its SDK 1.1 configuration: %+v, %v", configuration, configurationErr)
	}
	large := json.RawMessage(`"` + strings.Repeat("x", 8<<20) + `"`)
	written := make(chan error, 1)
	select {
	case lease.client.writes <- workerWrite{frame: sdk.Frame{Kind: "call", ID: "blocked-write", Method: sdk.MethodToolCall, Params: large}, done: written}:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture stdin did not accept the blocking write")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		select {
		case <-lease.client.readDone:
			t.Fatal("cancellation returned only after killing the worker")
		default:
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation blocked behind stdin")
	}
	select {
	case <-lease.client.done:
	case <-time.After(5 * time.Second):
		t.Fatal("unresponsive canceled worker was not retired")
	}
	select {
	case <-written:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked writer outlived its worker")
	}
}
