package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type gatewayFixtureTrace struct {
	Method  string          `json:"method"`
	Version string          `json:"version"`
	Scope   sdk.Scope       `json:"scope"`
	Auth    json.RawMessage `json:"auth,omitempty"`
	Params  json.RawMessage `json:"params"`
}

func gatewayFixtureModule() sdk.Module {
	data, err := os.ReadFile(sdk.ManifestFile)
	if err != nil {
		panic(err)
	}
	var manifest sdk.Manifest
	if json.Unmarshal(data, &manifest) != nil {
		panic("invalid gateway fixture manifest")
	}
	trace := func(ctx context.Context, call sdk.Call) error {
		current, ok := sdk.ModuleContextFrom(ctx)
		if !ok {
			return errors.New("missing configured gateway instance")
		}
		var config struct {
			Trace string `json:"trace"`
		}
		if json.Unmarshal(current.Config.Values, &config) != nil || config.Trace == "" {
			return errors.New("missing fixture trace path")
		}
		file, err := os.OpenFile(config.Trace, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		return json.NewEncoder(file).Encode(gatewayFixtureTrace{
			Method: call.Method, Version: manifest.Version, Scope: call.Scope, Auth: call.Auth, Params: call.Params,
		})
	}
	handlers := map[string]sdk.Handler{}
	if manifest.Hooks.Request {
		handlers[sdk.MethodGatewayRequest] = func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
			if err := trace(ctx, call); err != nil {
				return nil, err
			}
			var request sdk.GatewayRequest
			if json.Unmarshal(call.Params, &request) != nil {
				return nil, errors.New("invalid fixture recall request")
			}
			switch request.Context.Query {
			case "write-before-failure":
				var result sdk.StateCASResult
				_ = stream.HostCall(ctx, sdk.MethodStateCAS, sdk.StateCASRequest{
					Key: "recall.must-not-write", Version: "0", Value: json.RawMessage(`{"written":true}`),
				}, &result)
				return nil, errors.New("recall failed after attempting a persistent write")
			case "event":
				_ = stream.Emit(ctx, "data", []byte("hooks are unary"))
				return sdk.GatewayRequestResult{}, nil
			case "wait":
				<-ctx.Done()
				return nil, ctx.Err()
			case "failure":
				return nil, errors.New("private failure which must not be logged")
			case "large-state":
				state, _ := json.Marshal(strings.Repeat("x", sdk.MaxGatewayStateBytes+1))
				return sdk.GatewayRequestResult{State: state}, nil
			}
			var object map[string]any
			if json.Unmarshal(request.Body, &object) != nil {
				return nil, errors.New("invalid fixture body")
			}
			memory := "recalled fixture memory"
			if request.Context.Query == "from-store" {
				var state sdk.StateValue
				if err := stream.HostCall(ctx, sdk.MethodStateGet, sdk.StateGetRequest{Key: "memory.shared"}, &state); err != nil {
					return nil, err
				}
				var remembered struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(state.Value, &remembered); err != nil {
					return nil, err
				}
				memory = remembered.Text
			}
			if request.Context.Query == "protected" {
				object["model"] = "a-model-the-key-never-authorized"
			} else {
				messages, _ := object["messages"].([]any)
				object["messages"] = append([]any{map[string]any{"role": "system", "content": memory}}, messages...)
			}
			body, _ := json.Marshal(object)
			state, _ := json.Marshal(map[string]string{"version": manifest.Version, "query": request.Context.Query})
			return sdk.GatewayRequestResult{Body: body, State: state}, nil
		}
	}
	if manifest.Hooks.Response {
		handlers[sdk.MethodGatewayResponse] = func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
			var response sdk.GatewayResponse
			if json.Unmarshal(call.Params, &response) != nil {
				return nil, errors.New("invalid fixture capture")
			}
			if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
				return nil, err
			}
			if err := trace(ctx, call); err != nil {
				return nil, err
			}
			if response.Context.Query == "from-store" {
				var captured sdk.StateCASResult
				if err := stream.HostCall(ctx, sdk.MethodStateCAS, sdk.StateCASRequest{
					Key: "memory.captured", Version: "0", Value: json.RawMessage(`{"captured":true}`),
				}, &captured); err != nil {
					return nil, err
				}
			}
			if response.Context.Query == "wait-response" {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			// A domain result field called outcome is not a provider outcome.
			return map[string]string{"outcome": "accepted"}, nil
		}
	}
	if manifest.Hooks.SignIn {
		handlers[sdk.MethodGatewaySignIn] = func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
			if err := stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation}); err != nil {
				return nil, err
			}
			return struct{}{}, trace(ctx, call)
		}
	}
	if manifest.Contract.Has(sdk.ToolProvider) {
		handlers[sdk.MethodToolCall] = fixtureTool
	}
	return sdk.Module{Manifest: manifest, Handlers: handlers}
}

func gatewayFixturePackage(t *testing.T, version string, hooks sdk.GatewayHookDescriptor) string {
	t.Helper()
	directory := fixturePackage(t)
	data, err := os.ReadFile(filepath.Join(directory, sdk.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.ID, manifest.Version = "memory-fixture", version
	manifest.Args = []string{"--sdk-hook-worker"}
	manifest.Contract = sdk.Contract{
		Version: sdk.APIVersion{Major: 1, Minor: 2}, Capabilities: []sdk.Capability{sdk.GatewayHooks, sdk.ToolProvider},
	}
	manifest.Hooks = &hooks
	manifest.Inbounds, manifest.Providers, manifest.Agents, manifest.Sessions, manifest.Tools = nil, nil, nil, nil, nil
	manifest.Tools = []sdk.ToolDescriptor{{
		Name: "tool", InputSchema: json.RawMessage(`{"type":"object"}`), Exposure: "local-owner",
	}}
	catalogTestWriteManifest(t, directory, manifest)
	return directory
}

func gatewayFixtureManager(t *testing.T, hooks sdk.GatewayHookDescriptor) (*Manager, string) {
	t.Helper()
	manager := NewManager(t.TempDir())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.Install(t.Context(), gatewayFixturePackage(t, "1.0.0", hooks)); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "trace.jsonl")
	config, _ := json.Marshal(map[string]string{"trace": trace})
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), "memory-fixture", "0", config); err != nil {
		t.Fatal(err)
	}
	return manager, trace
}

func gatewayTrace(t *testing.T, path string) []gatewayFixtureTrace {
	t.Helper()
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var result []gatewayFixtureTrace
	for _, line := range bytes.Split(bytes.TrimSpace(body), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry gatewayFixtureTrace
		if err := json.Unmarshal(line, &entry); err != nil {
			// A concurrent fixture write may still be finishing. Its owner
			// waits on the run before requiring a complete final trace.
			continue
		}
		result = append(result, entry)
	}
	return result
}

func waitGatewayDone(t *testing.T, run *GatewayRun) {
	t.Helper()
	select {
	case <-run.done:
	case <-time.After(10 * time.Second):
		t.Fatal("gateway hook run did not release its generation")
	}
}

func TestGatewayHooksRecallCaptureSurvivesClientEOF(t *testing.T) {
	manager, trace := gatewayFixtureManager(t, sdk.GatewayHookDescriptor{Request: true, Response: true})
	ctx, cancel := context.WithCancel(t.Context())
	info := sdk.GatewayContext{Origin: "local", Protocol: "chat", Model: "m", Session: "conversation", Query: "preferences"}
	run := manager.BeginGateway(ctx, []byte(`{"model":"m","messages":[{"role":"user","content":"preferences"}]}`), info)
	if run == nil {
		t.Fatal("eligible recall hook was not called")
	}
	if !bytes.Contains(run.Body, []byte("recalled fixture memory")) {
		t.Fatalf("missing recalled context: %s", run.Body)
	}
	cancel() // The client has read a complete reply and closed its connection.
	run.Finish(ctx, json.RawMessage(`{"choices":[{"message":{"content":"done"}}]}`), true)
	run.Close()
	waitGatewayDone(t, run)
	events := gatewayTrace(t, trace)
	if len(events) != 2 || events[1].Method != sdk.MethodGatewayResponse {
		t.Fatalf("completion did not retain its owned lifetime: %+v", events)
	}
	var response sdk.GatewayResponse
	if json.Unmarshal(events[1].Params, &response) != nil || !response.Success ||
		!bytes.Contains(response.State, []byte("preferences")) || !bytes.Equal(response.Request, run.Body) {
		t.Fatalf("capture lost request/state/success: %s", events[1].Params)
	}
	for _, event := range events {
		if len(event.Auth) != 0 || event.Scope.Account != "" || event.Scope.Provider != "" || event.Scope.Generation != 0 {
			t.Fatalf("a hook received account authority: %+v", event)
		}
	}
	journal, err := manager.Store().Operation("memory-fixture", events[1].Scope.Operation)
	if err != nil || journal.State != "completed" {
		t.Fatalf("domain outcome was read as provider execution state: %+v %v", journal, err)
	}
}

func TestGatewayHooksMemorySharesDurableStoreWithTools(t *testing.T) {
	manager, _ := gatewayFixtureManager(t, sdk.GatewayHookDescriptor{Request: true, Response: true})
	_, err := manager.Call(WithLocalOwner(t.Context(), true), "memory-fixture", sdk.MethodToolCall, sdk.Scope{Session: "agent-one"},
		sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(
			`{"action":"cas","key":"memory.shared","version":"0","value":{"text":"Remember the user's chosen format"}}`,
		)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := manager.BeginGateway(t.Context(), []byte(`{"model":"m","messages":[]}`),
		sdk.GatewayContext{Origin: "local", Protocol: "chat", Session: "agent-two", Query: "from-store"})
	if run == nil || !bytes.Contains(run.Body, []byte("Remember the user's chosen format")) {
		if run != nil {
			run.Close()
		}
		t.Fatal("a different agent could not recall the module's stored memory")
	}
	run.Finish(t.Context(), json.RawMessage(`{"choices":[{"message":{"content":"done"}}]}`), true)
	run.Close()
	waitGatewayDone(t, run)
	root := manager.root
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(root)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := restarted.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	result, err := restarted.Call(WithLocalOwner(t.Context(), true), "memory-fixture", sdk.MethodToolCall, sdk.Scope{Session: "agent-three"},
		sdk.ToolRequest{ID: "tool", Arguments: json.RawMessage(`{"action":"get","key":"memory.captured"}`)}, nil)
	var captured sdk.StateValue
	if err != nil || json.Unmarshal(result, &captured) != nil || !bytes.Contains(captured.Value, []byte(`"captured":true`)) {
		t.Fatalf("completion capture did not persist across restart: %s %v", result, err)
	}
}

func TestGatewayHooksKeepStateInTheOriginalGeneration(t *testing.T) {
	hooks := sdk.GatewayHookDescriptor{Request: true, Response: true}
	manager, trace := gatewayFixtureManager(t, hooks)
	body := []byte(`{"model":"m","messages":[]}`)
	info := sdk.GatewayContext{Origin: "local", Protocol: "chat", Model: "m"}
	old := manager.BeginGateway(t.Context(), body, info)
	if old == nil {
		t.Fatal("missing first generation")
	}
	defer old.Close()
	if _, err := manager.Install(t.Context(), gatewayFixturePackage(t, "2.0.0", hooks)); err != nil {
		t.Fatal(err)
	}
	current := manager.BeginGateway(t.Context(), body, info)
	if current == nil {
		t.Fatal("missing replacement generation")
	}
	defer current.Close()
	old.Finish(t.Context(), json.RawMessage(`{}`), true)
	current.Finish(t.Context(), json.RawMessage(`{}`), true)
	waitGatewayDone(t, old)
	waitGatewayDone(t, current)
	var captures []gatewayFixtureTrace
	for _, event := range gatewayTrace(t, trace) {
		if event.Method == sdk.MethodGatewayResponse {
			captures = append(captures, event)
			var response sdk.GatewayResponse
			if json.Unmarshal(event.Params, &response) != nil || !bytes.Contains(response.State, []byte(event.Version)) {
				t.Fatalf("mixed recall/capture generations: %+v", event)
			}
		}
	}
	if len(captures) != 2 || captures[0].Version == captures[1].Version {
		t.Fatalf("did not capture both pinned generations: %+v", captures)
	}
}

func TestGatewayHooksSkipUnknownAndRemoteOriginsWithoutStarting(t *testing.T) {
	manager, trace := gatewayFixtureManager(t, sdk.GatewayHookDescriptor{Request: true, Response: true})
	for _, origin := range []string{"", "remote", "gateway-key", "local-owner-from-client"} {
		if run := manager.BeginGateway(t.Context(), []byte(`{"model":"m"}`), sdk.GatewayContext{Origin: origin}); run != nil {
			run.Close()
			t.Fatalf("unexpected private recall for origin %q", origin)
		}
	}
	if manager.Running("memory-fixture") || len(gatewayTrace(t, trace)) != 0 {
		t.Fatal("skipping an origin warmed the module or disclosed input")
	}
}

func TestGatewayHooksAcceptOnlyExplicitAdditionalOrigins(t *testing.T) {
	manager, _ := gatewayFixtureManager(t, sdk.GatewayHookDescriptor{Request: true, Origins: []string{"remote"}})
	if run := manager.BeginGateway(t.Context(), []byte(`{"model":"m"}`),
		sdk.GatewayContext{Origin: "local"}); run != nil {
		run.Close()
		t.Fatal("an explicit origin list was widened to the defaults")
	}
	run := manager.BeginGateway(t.Context(), []byte(`{"model":"m"}`), sdk.GatewayContext{Origin: "remote"})
	if run == nil {
		t.Fatal("explicit remote opt-in was ignored")
	}
	run.Close()
	waitGatewayDone(t, run)
}

func TestGatewayHooksRecallAboveTheHostCallbackLimit(t *testing.T) {
	manager, _ := gatewayFixtureManager(t, sdk.GatewayHookDescriptor{Request: true})
	body, _ := json.Marshal(map[string]any{
		"model": "m", "messages": []map[string]string{{"role": "user", "content": strings.Repeat("x", sdk.MaxHostBytes+1024)}},
	})
	run := manager.BeginGateway(t.Context(), body, sdk.GatewayContext{Origin: "local"})
	if run == nil {
		t.Fatal("valid long context was incorrectly limited to the smaller host-callback envelope")
	}
	defer run.Close()
	if len(run.Body) <= sdk.MaxHostBytes || !bytes.Contains(run.Body, []byte("recalled fixture memory")) {
		t.Fatal("long-context recall was truncated or skipped")
	}
}

func TestGatewayHooksOversizedRequestsDoNotAllocateOrWarm(t *testing.T) {
	manager := NewManager(t.TempDir())
	body := append([]byte(`{"images":"`), bytes.Repeat([]byte("x"), gatewayBodyLimit)...)
	body = append(body, '"', '}')
	ctx := context.Background()
	admitted := false
	allocations := testing.AllocsPerRun(3, func() {
		if run := manager.BeginGateway(ctx, body, sdk.GatewayContext{Origin: "local"}); run != nil {
			admitted = true
			run.Close()
		}
	})
	if admitted || allocations != 0 {
		t.Fatalf("oversized optional recall was admitted or allocated while skipping: admitted=%v allocations=%g", admitted, allocations)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.startCtx != nil || manager.closeDone != nil || len(manager.workers) != 0 {
		t.Fatal("oversized optional recall initialized the runtime")
	}
}

func TestGatewayHooksResponseOnlyAndSignIn(t *testing.T) {
	manager, trace := gatewayFixtureManager(t, sdk.GatewayHookDescriptor{Response: true, SignIn: true})
	body := []byte(`{"model":"m","messages":[]}`)
	run := manager.BeginGateway(t.Context(), body, sdk.GatewayContext{Origin: "magpie-key"})
	if run == nil || !bytes.Equal(run.Body, body) {
		t.Fatal("a response-only observer changed or rejected the request")
	}
	run.Finish(t.Context(), json.RawMessage(`{}`), true)
	run.Close()
	waitGatewayDone(t, run)
	ctx, cancel := context.WithCancel(t.Context())
	manager.GatewaySignIn(ctx, sdk.GatewaySignInEvent{
		Provider: "provider", ProviderName: "Subscription", Account: "account label", State: "signed-out",
	})
	cancel()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		events := gatewayTrace(t, trace)
		if slices.ContainsFunc(events, func(event gatewayFixtureTrace) bool { return event.Method == sdk.MethodGatewaySignIn }) {
			if len(events) != 2 || events[0].Method != sdk.MethodGatewayResponse {
				t.Fatalf("response-only hook received recall or duplicate notification: %+v", events)
			}
			break
		}
		select {
		case <-timer.C:
			t.Fatal("owned sign-in notification was canceled with its sender")
		case <-tick.C:
		}
	}
}

func TestGatewayHooksRollBackFailures(t *testing.T) {
	for _, query := range []string{"failure", "large-state", "protected", "wait", "event"} {
		t.Run(query, func(t *testing.T) {
			manager, trace := gatewayFixtureManager(t, sdk.GatewayHookDescriptor{
				Request: true, Response: true, TimeoutMs: 100,
			})
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if run := manager.BeginGateway(ctx, []byte(`{"model":"m","messages":[]}`),
				sdk.GatewayContext{Origin: "local", Query: query}); run != nil {
				run.Close()
				t.Fatal("failed or unauthorized enrichment was accepted")
			}
			events := gatewayTrace(t, trace)
			if len(events) != 1 || events[0].Method != sdk.MethodGatewayRequest {
				t.Fatalf("fixture did not exercise the rejected request: %+v", events)
			}
			for _, event := range events {
				if event.Method == sdk.MethodGatewayResponse {
					t.Fatal("a rolled-back request received completion")
				}
			}
		})
	}
}

func TestGatewayHooksNeverDelegateAccountAuthority(t *testing.T) {
	for _, method := range []string{sdk.MethodGatewayRequest, sdk.MethodGatewayResponse, sdk.MethodGatewaySignIn} {
		for _, scope := range []sdk.Scope{{Provider: "p"}, {Account: "a", Generation: 1}} {
			if err := validateGatewayScope(method, scope); err == nil {
				t.Fatalf("%s accepted account scope %+v", method, scope)
			}
		}
	}
}

func TestGatewayHooksProtectClientRoutingAndTools(t *testing.T) {
	for _, field := range []string{"model", "stream", "tools", "tool_choice", "function_call", "functions", "toolConfig"} {
		t.Run(field, func(t *testing.T) {
			before := map[string]any{field: "original"}
			after := map[string]any{field: "changed"}
			a, _ := json.Marshal(before)
			b, _ := json.Marshal(after)
			if gatewayHookChange(a, b) || gatewayHookChange([]byte(`{}`), b) || gatewayHookChange(a, []byte(`{}`)) {
				t.Fatal("protected value/presence changed")
			}
		})
	}
	for _, field := range []string{"tools", "toolConfig"} {
		a, _ := json.Marshal(map[string]any{"config": map[string]any{field: []string{"client-tool"}}})
		if gatewayHookChange(a, []byte(`{"config":{}}`)) {
			t.Fatalf("removed config.%s", field)
		}
	}
	for _, container := range []string{"tools", "config"} {
		before := []byte(`{"tools":[{"schema":{"minimum":9007199254740993}}]}`)
		after := []byte(`{"tools":[{"schema":{"minimum":9007199254740992}}]}`)
		if container == "config" {
			before = append(append([]byte(`{"config":`), before...), '}')
			after = append(append([]byte(`{"config":`), after...), '}')
		}
		if gatewayHookChange(before, after) {
			t.Fatalf("large numeric tool value was rounded through %s", container)
		}
	}
	if !gatewayHookChange([]byte(`{"model":"m","messages":[]}`),
		[]byte(`{"model":"m","messages":[{"role":"system","content":"recalled"}]}`)) {
		t.Fatal("context enrichment was rejected")
	}
}

func TestGatewayHooksFailedRecallCannotPersistWrites(t *testing.T) {
	manager, trace := gatewayFixtureManager(t, sdk.GatewayHookDescriptor{Request: true})
	if run := manager.BeginGateway(t.Context(), []byte(`{"model":"m"}`),
		sdk.GatewayContext{Origin: "local", Query: "write-before-failure"}); run != nil {
		run.Close()
		t.Fatal("failed recall was accepted")
	}
	events := gatewayTrace(t, trace)
	if len(events) != 1 || events[0].Method != sdk.MethodGatewayRequest {
		t.Fatal("the fixture did not attempt its recall")
	}
	state, err := manager.Store().StateGet(sdk.Scope{Plugin: "memory-fixture"}, "recall.must-not-write")
	if err != nil || state.Version != 0 || len(state.Value) != 0 {
		t.Fatalf("rolled-back recall persisted state: %+v %v", state, err)
	}
}

func TestGatewayHooksRecallRestrictionSurvivesReadDelegation(t *testing.T) {
	manifest := componentFixtureManifest()
	fixture := simpleComponentFixture(t, manifest)
	manager := fixture.group.manager
	if err := fixture.group.prepare(t.Context(), fixture.primary); err != nil {
		t.Fatal(err)
	}
	called := false
	fixture.action = func(ctx context.Context, call componentInvocation, _ func(Event) error) (json.RawMessage, error) {
		called = true
		body := json.RawMessage(`{"key":"recall.must-not-write","version":"0","value":true}`)
		value, err := manager.hostCall(ctx, call.scope, sdk.MethodStateCAS, body)
		if err != nil {
			return nil, err
		}
		return json.Marshal(value)
	}
	ctx := context.WithValue(t.Context(), gatewayReadOnlyKey{}, true)
	ctx = fixture.group.withAuthority(ctx, sdk.PrimaryComponent, "read")
	ctx = context.WithValue(ctx, hostAuthorityKey{}, hostAuthority{manifest: manifest, method: sdk.MethodGatewayRequest})
	if _, err := fixture.group.callBinding(ctx, sdk.PrimaryComponent, "controller.read",
		sdk.Scope{Plugin: manifest.ID}, nil, json.RawMessage(`{}`)); err == nil {
		t.Fatal("delegating recall to a read helper lost the persistent-write restriction")
	}
	if !called {
		t.Fatal("the read helper was not actually invoked")
	}
}

func TestGatewayHooksShutdownJoinsOwnedCapture(t *testing.T) {
	manager, trace := gatewayFixtureManager(t, sdk.GatewayHookDescriptor{Request: true, Response: true, TimeoutMs: 30000})
	run := manager.BeginGateway(t.Context(), []byte(`{"model":"m","messages":[]}`),
		sdk.GatewayContext{Origin: "local", Query: "wait-response"})
	if run == nil {
		t.Fatal("missing hook run")
	}
	run.Finish(t.Context(), json.RawMessage(`{}`), true)
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !slices.ContainsFunc(gatewayTrace(t, trace), func(event gatewayFixtureTrace) bool {
		return event.Method == sdk.MethodGatewayResponse
	}) {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("capture did not start")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	waitGatewayDone(t, run)
}

func TestGatewayHooksBoundAbandonedRuns(t *testing.T) {
	manager := NewManager(t.TempDir())
	for index := 0; index < maxGatewayRuns; index++ {
		if run := manager.openGatewayRun(t.Context(), nil, sdk.GatewayContext{}, time.Minute); run == nil {
			t.Fatal("run capacity was lost early")
		}
	}
	if run := manager.openGatewayRun(t.Context(), nil, sdk.GatewayContext{}, time.Minute); run != nil {
		run.Close()
		t.Fatal("unbounded run admission")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.gatewayRuns) != 0 {
		t.Fatal("shutdown retained abandoned hook runs")
	}
}
