package extension

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func actionModule(handler Handler) Module {
	manifest := compositeManifest()
	return Module{Manifest: manifest, Actions: map[string]Handler{"refresh": handler}}
}

func configuration(manifest Manifest, version string) ComponentConfigureRequest {
	contract := ServiceContract{}
	if manifest.Component != nil {
		contract = manifest.Component.Contract
	}
	return ComponentConfigureRequest{
		Component: componentID(manifest), Contract: contract, Instance: "instance-1",
		Config: ConfigSnapshot{Version: version, Values: json.RawMessage(`{"enabled":true}`)},
	}
}

func configurePeer(t *testing.T, peer *testPeer, manifest Manifest, version string) ComponentReadyResult {
	t.Helper()
	peer.send(callFrame("configure", MethodComponentConfigure, configuration(manifest, version)))
	frame := peer.read()
	var ready ComponentReadyResult
	if frame.ID != "configure" || frame.Error != nil || json.Unmarshal(frame.Result, &ready) != nil {
		t.Fatalf("configure failed: %+v", frame)
	}
	return ready
}

func waitLifecycle(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("component hook did not finish")
	}
}

func TestModuleActionsAreDeclaredSDKOwnedAndComponentLocal(t *testing.T) {
	module := actionModule(testNoop)
	if err := module.Validate(); err != nil {
		t.Fatal(err)
	}
	delete(module.Actions, "refresh")
	if err := module.Validate(); err == nil {
		t.Fatal("advertised action lacks a handler")
	}
	module.Actions["refresh"] = testNoop
	for _, id := range []string{"unknown", "backend.run"} {
		module.Actions[id] = testNoop
		if err := module.Validate(); err == nil {
			t.Fatal("unowned action registered:", id)
		}
		delete(module.Actions, id)
	}
	module.Handlers = map[string]Handler{MethodModuleCall: testNoop}
	if err := module.Validate(); err == nil {
		t.Fatal("author replaced SDK-owned dispatch")
	}
	module.Handlers = map[string]Handler{MethodComponentConfigure: testNoop}
	if err := module.Validate(); err == nil {
		t.Fatal("author replaced SDK-owned configure")
	}
	legacy := operationModule(testNoop)
	legacy.Start = func(context.Context, ModuleContext) (ModuleReady, func(), error) { return ModuleReady{}, nil, nil }
	if err := legacy.Validate(); err == nil {
		t.Fatal("API 1.0 advertised an API 1.1 lifecycle hook")
	}
}

func TestComponentConfigureStartsOnceAndFreezesContextUntilDispose(t *testing.T) {
	var starts, disposals atomic.Int32
	disposed := make(chan struct{})
	endpoints := map[string]string{"ui": "http://127.0.0.1:12345/"}
	var hookCtx context.Context
	module := actionModule(func(ctx context.Context, call Call, stream *Stream) (any, error) {
		value, found := ModuleContextFrom(ctx)
		if !found || call.Action != "refresh" || string(call.Params) != `{"x":1}` {
			return nil, errors.New("missing direct action or frozen module context")
		}
		original := string(value.Config.Values)
		value.Config.Values[0] = '!'
		second, _ := ModuleContextFrom(ctx)
		if string(second.Config.Values) != original {
			return nil, errors.New("context accessor shared mutable configuration")
		}
		return second.Config, nil
	})
	module.Start = func(ctx context.Context, value ModuleContext) (ModuleReady, func(), error) {
		starts.Add(1)
		hookCtx = ctx
		value.Config.Values[0] = '!'
		return ModuleReady{Endpoints: endpoints}, func() {
			disposals.Add(1)
			close(disposed)
		}, nil
	}
	peer := startPeer(t, module)
	peer.send(callFrame("describe", MethodDescribe, nil))
	if frame := peer.read(); frame.Error != nil || starts.Load() != 0 {
		t.Fatal("describe started the module")
	}
	peer.send(callFrame("before", MethodModuleCall, ModuleCallRequest{Action: "refresh", Input: json.RawMessage(`{}`)}))
	if frame := peer.read(); frame.Error == nil || frame.Error.Code != "not_ready" {
		t.Fatal("unconfigured component accepted an action:", frame)
	}
	ready := configurePeer(t, peer, module.Manifest, "18446744073709551615")
	if ready.Component != PrimaryComponent || ready.ConfigVersion != "18446744073709551615" || ready.Endpoints["ui"] != endpoints["ui"] {
		t.Fatal("readiness lost actual configuration:", ready)
	}
	if hookCtx.Err() != nil {
		t.Fatal("startup hook inherited the configure call's temporary lifetime")
	}
	endpoints["ui"] = "http://127.0.0.1:54321/"
	configurePeer(t, peer, module.Manifest, "18446744073709551615")
	if starts.Load() != 1 {
		t.Fatal("identical configure reran startup")
	}
	peer.send(callFrame("ready", MethodComponentReady, struct{}{}))
	frame := peer.read()
	var repeated ComponentReadyResult
	if frame.Error != nil || json.Unmarshal(frame.Result, &repeated) != nil || !reflect.DeepEqual(repeated, ready) {
		t.Fatal("readiness retained a mutable hook result:", frame)
	}
	peer.send(callFrame("changed", MethodComponentConfigure, configuration(module.Manifest, "2")))
	if frame := peer.read(); frame.Error == nil || frame.Error.Code != "conflict" {
		t.Fatal("runtime accepted a mutable configuration:", frame)
	}
	for _, action := range []string{"refresh", "backend.run", "unknown"} {
		peer.send(callFrame(action, MethodModuleCall, ModuleCallRequest{Action: action, Input: json.RawMessage(`{"x":1}`)}))
		frame := peer.read()
		if action == "refresh" {
			if frame.Error != nil || string(frame.Result) != `{"version":"18446744073709551615","values":{"enabled":true}}` {
				t.Fatal("action did not receive direct JSON and frozen config:", frame)
			}
		} else if frame.Error == nil || frame.Error.Code != "method_not_found" {
			t.Fatal("primary dispatched an undeclared or helper-owned action:", frame)
		}
	}
	peer.cancel()
	_ = peer.wait()
	waitLifecycle(t, disposed)
	if disposals.Load() != 1 || hookCtx.Err() == nil {
		t.Fatal("module lifetime did not cancel and dispose exactly once")
	}
}

func TestComponentStateIsSharedWithinInstanceAndIsolatedBetweenServeLifetimes(t *testing.T) {
	type ComponentState struct {
		Instance string
		Calls    atomic.Uint64
		Opaque   chan struct{}
	}
	module := actionModule(func(ctx context.Context, _ Call, _ *Stream) (any, error) {
		value, _ := ModuleContextFrom(ctx)
		state, ok := value.State.(*ComponentState)
		if !ok || state.Instance != value.Instance {
			return nil, errors.New("missing or foreign component state")
		}
		again, _ := ModuleContextFrom(ctx)
		if again.State != state {
			return nil, errors.New("opaque state was copied within an instance")
		}
		return map[string]any{"instance": state.Instance, "calls": state.Calls.Add(1)}, nil
	})
	module.Start = func(_ context.Context, value ModuleContext) (ModuleReady, func(), error) {
		if value.State != nil {
			return ModuleReady{}, nil, errors.New("startup inherited state from another instance")
		}
		state := &ComponentState{Instance: value.Instance, Opaque: make(chan struct{})}
		return ModuleReady{State: state}, func() { close(state.Opaque) }, nil
	}
	first, second := startPeer(t, module), startPeer(t, module)
	for _, instance := range []struct {
		peer *testPeer
		id   string
	}{{first, "first"}, {second, "second"}} {
		request := configuration(module.Manifest, "1")
		request.Instance = instance.id
		instance.peer.send(callFrame("configure", MethodComponentConfigure, request))
		ready := instance.peer.read()
		if ready.Error != nil || strings.Contains(string(ready.Result), `"State"`) || strings.Contains(string(ready.Result), `"state"`) {
			t.Fatal("opaque component state reached readiness JSON:", ready)
		}
	}
	for count := uint64(1); count <= 2; count++ {
		for _, instance := range []struct {
			peer *testPeer
			id   string
		}{{first, "first"}, {second, "second"}} {
			instance.peer.send(callFrame("action-"+strconv.FormatUint(count, 10), MethodModuleCall,
				ModuleCallRequest{Action: "refresh", Input: json.RawMessage(`{}`)}))
			frame := instance.peer.read()
			var actual struct {
				Instance string `json:"instance"`
				Calls    uint64 `json:"calls"`
			}
			if frame.Error != nil || json.Unmarshal(frame.Result, &actual) != nil || actual.Instance != instance.id || actual.Calls != count {
				t.Fatal("Serve instances shared or lost their Go state:", frame)
			}
		}
	}
}

func TestModuleStateIsNeverSerializedWithContextOrReadiness(t *testing.T) {
	state := make(chan struct{})
	for _, value := range []any{ModuleReady{State: state}, ModuleContext{State: state}} {
		body, err := json.Marshal(value)
		if err != nil || strings.Contains(string(body), `"State"`) || strings.Contains(string(body), `"state"`) {
			t.Fatal("nonserializable Go state leaked into JSON:", string(body), err)
		}
	}
}

func TestComponentConcurrentConfigureAndReadinessNeverPretendStartupSucceeded(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var starts atomic.Int32
	module := actionModule(testNoop)
	module.Start = func(ctx context.Context, value ModuleContext) (ModuleReady, func(), error) {
		starts.Add(1)
		close(started)
		select {
		case <-release:
			return ModuleReady{}, nil, nil
		case <-ctx.Done():
			return ModuleReady{}, nil, ctx.Err()
		}
	}
	peer := startPeer(t, module)
	request := configuration(module.Manifest, "1")
	peer.send(callFrame("first", MethodComponentConfigure, request))
	waitLifecycle(t, started)
	peer.send(callFrame("second", MethodComponentConfigure, request))
	peer.send(callFrame("not-yet", MethodComponentReady, nil))
	if frame := peer.read(); frame.ID != "not-yet" || frame.Error == nil || frame.Error.Code != "not_ready" {
		t.Fatal("component claimed readiness before startup completed:", frame)
	}
	close(release)
	seen := map[string]bool{}
	for range 2 {
		frame := peer.read()
		if frame.Error != nil {
			t.Fatal("concurrent identical configure failed:", frame)
		}
		seen[frame.ID] = true
	}
	if !seen["first"] || !seen["second"] || starts.Load() != 1 {
		t.Fatal("configuration was not initialized once:", seen)
	}
}

func TestComponentConfigurationRejectsInvalidSnapshotsBeforeStartup(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*ComponentConfigureRequest)
	}{
		{"component", func(r *ComponentConfigureRequest) { r.Component = "another" }},
		{"contract", func(r *ComponentConfigureRequest) { r.Contract = ServiceContract{ID: "other", Version: 1} }},
		{"instance empty", func(r *ComponentConfigureRequest) { r.Instance = "" }},
		{"instance whitespace", func(r *ComponentConfigureRequest) { r.Instance = "two words" }},
		{"version empty", func(r *ComponentConfigureRequest) { r.Config.Version = "" }},
		{"version leading zero", func(r *ComponentConfigureRequest) { r.Config.Version = "01" }},
		{"version negative", func(r *ComponentConfigureRequest) { r.Config.Version = "-1" }},
		{"version overflow", func(r *ComponentConfigureRequest) { r.Config.Version = "18446744073709551616" }},
		{"values bound", func(r *ComponentConfigureRequest) {
			r.Config.Values = json.RawMessage(`"` + strings.Repeat("x", MaxHostBytes) + `"`)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var starts atomic.Int32
			module := actionModule(testNoop)
			module.Start = func(context.Context, ModuleContext) (ModuleReady, func(), error) {
				starts.Add(1)
				return ModuleReady{}, nil, nil
			}
			peer := startPeer(t, module)
			request := configuration(module.Manifest, "0")
			test.edit(&request)
			peer.send(callFrame("bad", MethodComponentConfigure, request))
			if frame := peer.read(); frame.Error == nil || frame.Error.Code != "invalid_params" || starts.Load() != 0 {
				t.Fatal("invalid configure reached startup:", frame)
			}
			configurePeer(t, peer, module.Manifest, "0")
			if starts.Load() != 1 {
				t.Fatal("invalid request consumed the runtime's initialization")
			}
		})
	}
}

func TestServiceConfigureUsesItsExactContractAndActionOwnership(t *testing.T) {
	view, err := ServiceManifest(compositeManifest(), "backend")
	if err != nil {
		t.Fatal(err)
	}
	module := Module{Manifest: view, Actions: map[string]Handler{
		"backend.run": func(ctx context.Context, call Call, _ *Stream) (any, error) {
			value, _ := ModuleContextFrom(ctx)
			if value.Component != "backend" || call.Action != "backend.run" {
				return nil, errors.New("wrong component")
			}
			return value.Config.Version, nil
		},
	}}
	peer := startPeer(t, module)
	wrong := configuration(view, "3")
	wrong.Contract.Version++
	peer.send(callFrame("wrong", MethodComponentConfigure, wrong))
	if frame := peer.read(); frame.Error == nil || frame.Error.Code != "invalid_params" {
		t.Fatal("service accepted a different named contract version:", frame)
	}
	ready := configurePeer(t, peer, view, "3")
	if ready.Component != "backend" || ready.Contract != view.Component.Contract || ready.Instance != "instance-1" {
		t.Fatal("service fabricated a readiness identity:", ready)
	}
	peer.send(callFrame("backend", MethodModuleCall, ModuleCallRequest{Action: "backend.run", Input: json.RawMessage(`{}`)}))
	if frame := peer.read(); frame.Error != nil || string(frame.Result) != `"3"` {
		t.Fatal("service did not dispatch its owned action:", frame)
	}
	peer.send(callFrame("primary", MethodModuleCall, ModuleCallRequest{Action: "refresh", Input: json.RawMessage(`{}`)}))
	if frame := peer.read(); frame.Error == nil || frame.Error.Code != "method_not_found" {
		t.Fatal("service dispatched the primary's action:", frame)
	}
}

func dependencyReady() ComponentReadyResult {
	return ComponentReadyResult{
		Component: "storage", Contract: ServiceContract{ID: "storage-contract", Version: 1},
		Instance: "storage-instance", ConfigVersion: "4",
		Endpoints: map[string]string{"http": "http://127.0.0.2:12345/"},
	}
}

func TestConfiguredDependenciesAreAvailableAndDeeplyFrozenInServiceContexts(t *testing.T) {
	view, err := ServiceManifest(compositeManifest(), "backend")
	if err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int32
	module := Module{Manifest: view}
	module.Start = func(_ context.Context, value ModuleContext) (ModuleReady, func(), error) {
		starts.Add(1)
		if !reflect.DeepEqual(value.Services["storage"], dependencyReady()) {
			return ModuleReady{}, nil, errors.New("missing dependency readiness")
		}
		value.Services["storage"].Endpoints["http"] = "http://127.0.0.1:2/"
		delete(value.Services, "storage")
		return ModuleReady{}, nil, nil
	}
	module.Actions = map[string]Handler{"backend.run": func(ctx context.Context, _ Call, _ *Stream) (any, error) {
		value, _ := ModuleContextFrom(ctx)
		if !reflect.DeepEqual(value.Services["storage"], dependencyReady()) {
			return nil, errors.New("startup or another call mutated dependency context")
		}
		value.Services["storage"].Endpoints["http"] = "http://127.0.0.1:3/"
		delete(value.Services, "storage")
		independent, _ := ModuleContextFrom(ctx)
		return independent.Services["storage"], nil
	}}
	peer := startPeer(t, module)
	request := configuration(view, "4")
	request.Services = map[string]ComponentReadyResult{"storage": dependencyReady()}
	for _, id := range []string{"first", "identical"} {
		peer.send(callFrame(id, MethodComponentConfigure, request))
		if frame := peer.read(); frame.Error != nil {
			t.Fatal("frozen dependency context did not configure:", frame)
		}
	}
	if starts.Load() != 1 {
		t.Fatal("identical dependency snapshots reran startup")
	}
	for _, id := range []string{"action-1", "action-2"} {
		peer.send(callFrame(id, MethodModuleCall, ModuleCallRequest{Action: "backend.run", Input: json.RawMessage(`{}`)}))
		frame := peer.read()
		var result ComponentReadyResult
		if frame.Error != nil || json.Unmarshal(frame.Result, &result) != nil || !reflect.DeepEqual(result, dependencyReady()) {
			t.Fatal("dependency readiness was shared or unavailable:", frame)
		}
	}
	changed := dependencyReady()
	changed.Instance = "replacement-instance"
	request.Services["storage"] = changed
	peer.send(callFrame("changed", MethodComponentConfigure, request))
	if frame := peer.read(); frame.Error == nil || frame.Error.Code != "conflict" {
		t.Fatal("running component accepted replacement dependency identity:", frame)
	}
}

func TestConfiguredDependencyRecordsRequireMatchingIdentityVersionAndLoopbackEndpoints(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*ComponentReadyResult)
	}{
		{"component", func(r *ComponentReadyResult) { r.Component = "other" }},
		{"contract id", func(r *ComponentReadyResult) { r.Contract.ID = "" }},
		{"contract version", func(r *ComponentReadyResult) { r.Contract.Version = 0 }},
		{"instance empty", func(r *ComponentReadyResult) { r.Instance = "" }},
		{"instance control", func(r *ComponentReadyResult) { r.Instance = "bad\ninstance" }},
		{"config version", func(r *ComponentReadyResult) { r.ConfigVersion = "3" }},
		{"endpoint name", func(r *ComponentReadyResult) { r.Endpoints = map[string]string{"../http": "http://127.0.0.1:1/"} }},
		{"remote endpoint", func(r *ComponentReadyResult) { r.Endpoints["http"] = "https://vendor.invalid:443/" }},
		{"credentials", func(r *ComponentReadyResult) { r.Endpoints["http"] = "http://secret@127.0.0.1:12345/" }},
		{"fragment", func(r *ComponentReadyResult) { r.Endpoints["http"] = "http://127.0.0.1:12345/#fragment" }},
		{"port zero", func(r *ComponentReadyResult) { r.Endpoints["http"] = "http://127.0.0.1:0/" }},
		{"port range", func(r *ComponentReadyResult) { r.Endpoints["http"] = "http://127.0.0.1:65536/" }},
		{"scheme", func(r *ComponentReadyResult) { r.Endpoints["http"] = "ws://127.0.0.1:12345/" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			peer := startPeer(t, actionModule(testNoop))
			request := configuration(compositeManifest(), "4")
			ready := dependencyReady()
			test.edit(&ready)
			request.Services = map[string]ComponentReadyResult{"storage": ready}
			peer.send(callFrame("invalid", MethodComponentConfigure, request))
			if frame := peer.read(); frame.Error == nil || frame.Error.Code != "invalid_params" {
				t.Fatal("invalid dependency readiness record was accepted:", frame)
			}
		})
	}
}

func TestComponentStartupFailureAndCancellationDisposeOwnedResources(t *testing.T) {
	for _, mode := range []string{"error", "panic", "bad-endpoint", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			started, disposed := make(chan struct{}), make(chan struct{})
			var count atomic.Int32
			module := actionModule(testNoop)
			module.Start = func(ctx context.Context, _ ModuleContext) (ModuleReady, func(), error) {
				close(started)
				dispose := func() { count.Add(1); close(disposed) }
				switch mode {
				case "panic":
					panic("private credential")
				case "cancel":
					<-ctx.Done()
					return ModuleReady{}, dispose, ctx.Err()
				case "bad-endpoint":
					return ModuleReady{Endpoints: map[string]string{"ui": "https://vendor.invalid/"}}, dispose, nil
				default:
					return ModuleReady{}, dispose, errors.New("private credential")
				}
			}
			peer := startPeer(t, module)
			peer.send(callFrame("setup", MethodComponentConfigure, configuration(module.Manifest, "1")))
			waitLifecycle(t, started)
			if mode == "cancel" {
				peer.send(Frame{Kind: KindCancel, ID: "setup"})
			}
			frame := peer.read()
			if frame.Error == nil || frame.Error.Message == "private credential" {
				t.Fatal("failed startup fabricated readiness or leaked a private error:", frame)
			}
			if mode != "panic" {
				waitLifecycle(t, disposed)
				if count.Load() != 1 {
					t.Fatal("failed startup did not dispose exactly once")
				}
			}
			peer.send(callFrame("ready", MethodComponentReady, nil))
			if frame := peer.read(); frame.Error == nil {
				t.Fatal("failed startup later reported readiness")
			}
			peer.cancel()
			_ = peer.wait()
			if mode != "panic" && count.Load() != 1 {
				t.Fatal("shutdown disposed failed startup a second time")
			}
		})
	}
}

func TestComponentLateStartupIsDisposedAfterParentAndServerCancel(t *testing.T) {
	started, release, disposed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	module := actionModule(testNoop)
	module.Start = func(context.Context, ModuleContext) (ModuleReady, func(), error) {
		close(started)
		<-release
		return ModuleReady{}, func() { close(disposed) }, nil
	}
	peer := startPeer(t, module)
	t.Cleanup(func() { peer.cancel(); close(release); waitLifecycle(t, disposed) })
	peer.send(callFrame("setup", MethodComponentConfigure, configuration(module.Manifest, "1")))
	waitLifecycle(t, started)
	peer.send(Frame{Kind: KindCancel, ID: "setup"})
	if frame := peer.read(); frame.Error == nil || frame.Error.Code != "canceled" {
		t.Fatal("configure waited for an uncooperative startup hook")
	}
	peer.cancel()
	_ = peer.wait()
}

func TestComponentStopCancelsAndDisposesBeforeEveryReplyWithoutClosingWire(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var contextOfHook context.Context
	var count atomic.Int32
	module := actionModule(testNoop)
	module.Start = func(ctx context.Context, _ ModuleContext) (ModuleReady, func(), error) {
		contextOfHook = ctx
		return ModuleReady{}, func() {
			count.Add(1)
			close(entered)
			<-release
		}, nil
	}
	peer := startPeer(t, module)
	configurePeer(t, peer, module.Manifest, "1")
	peer.send(callFrame("stop-1", MethodComponentStop, nil))
	waitLifecycle(t, entered)
	if contextOfHook.Err() == nil {
		t.Fatal("disposer ran before component cancellation")
	}
	peer.send(callFrame("stop-2", MethodComponentStop, struct{}{}))
	peer.send(callFrame("barrier", MethodDescribe, nil))
	if frame := peer.read(); frame.ID != "barrier" || frame.Error != nil {
		t.Fatal("Stop replied before its disposer finished or closed the wire:", frame)
	}
	close(release)
	seen := map[string]bool{}
	for range 2 {
		frame := peer.read()
		if frame.Error != nil || frame.Kind != KindResult {
			t.Fatal("Stop did not acknowledge completed disposal:", frame)
		}
		seen[frame.ID] = true
	}
	if !seen["stop-1"] || !seen["stop-2"] || count.Load() != 1 {
		t.Fatal("concurrent Stop repeated disposal:", seen, count.Load())
	}
	peer.send(callFrame("stop-3", MethodComponentStop, nil))
	if frame := peer.read(); frame.Error != nil {
		t.Fatal("repeated Stop failed:", frame)
	}
	peer.send(callFrame("ready-after-stop", MethodComponentReady, nil))
	if frame := peer.read(); frame.Error == nil {
		t.Fatal("stopped component reported readiness")
	}
	peer.send(callFrame("configure-after-stop", MethodComponentConfigure, configuration(module.Manifest, "1")))
	if frame := peer.read(); frame.Error == nil {
		t.Fatal("stopped component configured again")
	}
	peer.cancel()
	_ = peer.wait()
	if count.Load() != 1 {
		t.Fatal("transport close disposed stopped resources twice")
	}
}

func TestComponentStopBeforeConfigurePreventsStartup(t *testing.T) {
	var starts atomic.Int32
	module := actionModule(testNoop)
	module.Start = func(context.Context, ModuleContext) (ModuleReady, func(), error) {
		starts.Add(1)
		return ModuleReady{}, nil, nil
	}
	peer := startPeer(t, module)
	peer.send(callFrame("stop", MethodComponentStop, nil))
	if frame := peer.read(); frame.Error != nil {
		t.Fatal("Stop before Configure failed:", frame)
	}
	peer.send(callFrame("configure", MethodComponentConfigure, configuration(module.Manifest, "0")))
	if frame := peer.read(); frame.Error == nil || starts.Load() != 0 {
		t.Fatal("Stop before Configure allowed startup:", frame)
	}
	peer.send(callFrame("ready", MethodComponentReady, nil))
	if frame := peer.read(); frame.Error == nil {
		t.Fatal("Stop before Configure fabricated readiness")
	}
	peer.send(callFrame("describe", MethodDescribe, nil))
	if frame := peer.read(); frame.Error != nil {
		t.Fatal("Stop before Configure closed the wire:", frame)
	}
}

func TestComponentStopContextCanEndWhileDisposerIsUncooperative(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	module := actionModule(testNoop)
	module.Start = func(context.Context, ModuleContext) (ModuleReady, func(), error) {
		return ModuleReady{}, func() { close(entered); <-release }, nil
	}
	peer := startPeer(t, module)
	t.Cleanup(func() { close(release) })
	configurePeer(t, peer, module.Manifest, "1")
	peer.send(callFrame("stop", MethodComponentStop, nil))
	waitLifecycle(t, entered)
	peer.send(Frame{Kind: KindCancel, ID: "stop"})
	if frame := peer.read(); frame.Error == nil || frame.Error.Code != "canceled" {
		t.Fatal("Stop ignored its bounded caller context:", frame)
	}
	peer.send(callFrame("describe", MethodDescribe, nil))
	if frame := peer.read(); frame.Error != nil {
		t.Fatal("a blocked disposer prevented unrelated wire control:", frame)
	}
}

func TestComponentStopWaitsForCanceledStartupCleanup(t *testing.T) {
	started, disposed := make(chan struct{}), make(chan struct{})
	module := actionModule(testNoop)
	module.Start = func(ctx context.Context, _ ModuleContext) (ModuleReady, func(), error) {
		close(started)
		<-ctx.Done()
		return ModuleReady{}, func() { close(disposed) }, ctx.Err()
	}
	peer := startPeer(t, module)
	peer.send(callFrame("configure", MethodComponentConfigure, configuration(module.Manifest, "1")))
	waitLifecycle(t, started)
	peer.send(callFrame("stop", MethodComponentStop, nil))
	seen := map[string]bool{}
	for range 2 {
		frame := peer.read()
		if frame.ID == "stop" {
			if frame.Error != nil {
				t.Fatal("Stop failed after cooperative startup cancellation:", frame)
			}
			select {
			case <-disposed:
			default:
				t.Fatal("Stop replied before canceled startup disposed resources")
			}
		} else if frame.ID != "configure" || frame.Error == nil {
			t.Fatal("canceled startup reported success:", frame)
		}
		seen[frame.ID] = true
	}
	if !seen["stop"] || !seen["configure"] {
		t.Fatal("missing control reply:", seen)
	}
}

func TestComponentStopCancelsActiveCallAndBackgroundStream(t *testing.T) {
	for _, callback := range []bool{false, true} {
		t.Run(map[bool]string{false: "context", true: "background-host-call"}[callback], func(t *testing.T) {
			started := make(chan struct{})
			module := actionModule(func(ctx context.Context, call Call, stream *Stream) (any, error) {
				if callback {
					return nil, stream.HostCall(context.Background(), MethodStateGet, StateGetRequest{Key: "state"}, nil)
				}
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			})
			peer := startPeer(t, module)
			configurePeer(t, peer, module.Manifest, "0")
			peer.send(callFrame("running", MethodModuleCall, ModuleCallRequest{Action: "refresh", Input: json.RawMessage(`{}`)}))
			if callback {
				if frame := peer.read(); frame.Kind != KindHostCall {
					t.Fatal("fixture did not enter the host callback:", frame)
				}
			} else {
				waitLifecycle(t, started)
			}
			peer.send(callFrame("stop", MethodComponentStop, nil))
			for range 2 {
				frame := peer.read()
				if frame.ID == "running" {
					if frame.Error == nil || frame.Error.Code != "canceled" {
						t.Fatal("component Stop did not cancel the active call:", frame)
					}
				} else if frame.ID != "stop" || frame.Error != nil {
					t.Fatal("component Stop did not complete:", frame)
				}
			}
		})
	}
}

func TestModuleRPCUnaryEffectsAndScopedComponentCallback(t *testing.T) {
	for _, effect := range []string{"read", "write"} {
		for _, event := range []string{"data", "progress", "accepted"} {
			t.Run(effect+"/"+event, func(t *testing.T) {
				module := actionModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
					data := any([]byte("unary"))
					if event == "accepted" {
						data = AcceptedEvent{OperationID: "root-operation"}
					}
					_ = stream.Emit(ctx, event, data)
					return json.RawMessage(`{"done":true}`), nil
				})
				module.Manifest.Actions[0].Effect = effect
				peer := startPeer(t, module)
				configurePeer(t, peer, module.Manifest, "0")
				peer.send(callFrame("action", MethodModuleCall, ModuleCallRequest{Action: "refresh", Input: json.RawMessage(`{}`)}))
				frame := peer.read()
				if effect == "write" && event == "accepted" {
					if frame.Kind != KindEvent || frame.Event != "accepted" {
						t.Fatal("write receipt was rejected:", frame)
					}
					peer.send(Frame{Kind: KindAck, ID: frame.ID, Seq: frame.Seq})
					frame = peer.read()
					if frame.Error != nil || string(frame.Result) != `{"done":true}` {
						t.Fatal("unary terminal JSON changed:", frame)
					}
				} else if frame.Kind != KindResult || frame.Error == nil || frame.Error.Code != "invalid_params" {
					t.Fatal("unary module RPC accepted an unsupported stream event:", frame)
				}
			})
		}
	}
	module := actionModule(func(ctx context.Context, _ Call, stream *Stream) (any, error) {
		var result json.RawMessage
		err := stream.HostCall(ctx, MethodComponentCall, ComponentCallRequest{
			Binding: "controller.run", Input: json.RawMessage(`{"job":1}`),
		}, &result)
		return result, err
	})
	module.Manifest.Actions[0].Effect = "write"
	peer := startPeer(t, module)
	configurePeer(t, peer, module.Manifest, "2")
	frame := callFrame("parent", MethodModuleCall, ModuleCallRequest{Action: "refresh", Input: json.RawMessage(`{}`)})
	frame.Scope.Account, frame.Scope.Generation, frame.Scope.Operation = "selected", 7, "root-operation"
	peer.send(frame)
	callback := peer.read()
	if callback.Kind != KindHostCall || callback.Method != MethodComponentCall ||
		callback.Parent != frame.ID || callback.Scope != frame.Scope {
		t.Fatal("component callback lost its pinned parent scope:", callback)
	}
	peer.send(Frame{Kind: KindHostResult, ID: callback.ID, Parent: frame.ID, Result: json.RawMessage(`{"job":1}`)})
	if terminal := peer.read(); terminal.Error != nil || string(terminal.Result) != `{"job":1}` {
		t.Fatal("helper callback did not return raw terminal JSON:", terminal)
	}
}
