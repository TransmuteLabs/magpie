package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func hookManifest(methods ...string) Manifest {
	manifest := testManifest(GatewayHooks)
	manifest.Contract.Version.Minor = 2
	manifest.Hooks = &GatewayHookDescriptor{}
	for _, method := range methods {
		switch method {
		case MethodGatewayRequest:
			manifest.Hooks.Request = true
		case MethodGatewayResponse:
			manifest.Hooks.Response = true
		case MethodGatewaySignIn:
			manifest.Hooks.SignIn = true
		}
	}
	return manifest
}

func hookContext() GatewayContext {
	return GatewayContext{
		Origin: "local", Protocol: "chat", Model: "fixture/model", Stream: true, Path: "/v1/chat/completions",
		Agent: "codex", Session: "conversation", Cwd: "/project", Query: "remember this", Kind: "subagent",
	}
}

func hookParams(method string) any {
	switch method {
	case MethodGatewayRequest:
		return GatewayRequest{Body: json.RawMessage(`{"model":"fixture/model","messages":[]}`), Context: hookContext()}
	case MethodGatewayResponse:
		return GatewayResponse{
			Request: json.RawMessage(`{"model":"fixture/model","messages":[]}`), Reply: json.RawMessage(`{"choices":[]}`),
			Success: true, State: json.RawMessage(`{"source":"remember"}`), Context: hookContext(),
		}
	default:
		return GatewaySignIn{Event: GatewaySignInEvent{
			Provider: "fixture", ProviderName: "Fixture", Account: "selected label", State: "ending", EndsAt: "2026-11-13T12:00:00Z",
		}}
	}
}

func hookPeer(t *testing.T, method string, handler Handler) *testPeer {
	t.Helper()
	module := Module{Manifest: hookManifest(method), Handlers: map[string]Handler{method: handler}}
	peer := startPeer(t, module)
	configurePeer(t, peer, module.Manifest, "0")
	return peer
}

func TestGatewayHookAdmissionRequiresAPI12CapabilityDescriptorAndExactMethods(t *testing.T) {
	for _, method := range []string{MethodGatewayRequest, MethodGatewayResponse, MethodGatewaySignIn} {
		t.Run(method, func(t *testing.T) {
			manifest := hookManifest(method)
			if err := manifest.Validate(); err != nil {
				t.Fatal(err)
			}
			if got := RequiredMethods(manifest); !reflect.DeepEqual(got, []string{method}) {
				t.Fatal("undeclared hook methods required:", got)
			}
			if cap, found := MethodCapability(method); !found || cap != GatewayHooks || !IsGatewayMethod(method) {
				t.Fatal("gateway hook lost its capability")
			}
			module := Module{Manifest: manifest}
			if err := module.Validate(); err == nil {
				t.Fatal("advertised hook without handler admitted")
			}
			module.Handlers = map[string]Handler{method: testNoop}
			if err := module.Validate(); err != nil {
				t.Fatal(err)
			}
			extra := MethodGatewayRequest
			if extra == method {
				extra = MethodGatewayResponse
			}
			module.Handlers[extra] = testNoop
			if err := module.Validate(); err == nil {
				t.Fatal("undeclared hook handler admitted")
			}
			for _, minor := range []int{0, 1} {
				old := manifest.Clone()
				old.Contract.Version.Minor = minor
				if err := old.Validate(); err == nil {
					t.Fatal("old API admitted new hook capability")
				}
			}
		})
	}
	for _, test := range []struct {
		name string
		edit func(*Manifest)
	}{
		{"descriptor absent", func(m *Manifest) { m.Hooks = nil }},
		{"capability absent", func(m *Manifest) { m.Contract.Capabilities = []Capability{OperationControl} }},
		{"no flags", func(m *Manifest) { m.Hooks.Request = false }},
		{"timeout negative", func(m *Manifest) { m.Hooks.TimeoutMs = -1 }},
		{"timeout small", func(m *Manifest) { m.Hooks.TimeoutMs = 99 }},
		{"timeout large", func(m *Manifest) { m.Hooks.TimeoutMs = 30001 }},
		{"unknown origin", func(m *Manifest) { m.Hooks.Origins = []string{"unknown"} }},
		{"duplicate origin", func(m *Manifest) { m.Hooks.Origins = []string{"local", "local"} }},
		{"duplicate browser", func(m *Manifest) { m.Hooks.Origins = []string{"browser", "browser"} }},
		{"signin without local", func(m *Manifest) {
			m.Hooks.SignIn, m.Hooks.Origins = true, []string{"magpie-key", "remote"}
		}},
		{"signin browser only", func(m *Manifest) {
			m.Hooks.SignIn, m.Hooks.Origins = true, []string{"browser"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := hookManifest(MethodGatewayRequest)
			test.edit(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatal("invalid hook declaration admitted")
			}
		})
	}
	for _, timeout := range []int{0, 100, 30000} {
		manifest := hookManifest(MethodGatewayRequest)
		manifest.Hooks.TimeoutMs = timeout
		if err := manifest.Validate(); err != nil {
			t.Fatal("valid hook timeout rejected:", timeout, err)
		}
	}
	for _, method := range []string{MethodDescribe, MethodModuleCall, MethodStateGet, "gateway.unknown"} {
		if IsGatewayMethod(method) {
			t.Fatal("unrelated method classified as gateway hook:", method)
		}
		if _, found := GatewayMethodEffect(method); found {
			t.Fatal("unrelated method received a gateway effect:", method)
		}
	}
	manifest := hookManifest(MethodGatewayRequest)
	manifest.Compatibility = &Compatibility{HostAPI: "^1.2"}
	if err := CheckCompatibility(manifest); err != nil {
		t.Fatal("API 1.2 compatibility rejected:", err)
	}
}

func TestGatewayOriginsAndSnapshotsKeepPrivateDefaultsAndPrimaryOwnership(t *testing.T) {
	root := hookManifest(MethodGatewayRequest, MethodGatewaySignIn)
	for _, origin := range []string{"local", "magpie-key", "gateway-key", "remote", "browser", "", "unknown"} {
		if root.Hooks.AllowsOrigin(origin) != (origin == "local" || origin == "magpie-key") {
			t.Fatal("private default origins changed:", origin)
		}
	}
	root.Hooks.Origins = []string{"local", "remote"}
	root.Services = []ServiceDescriptor{{ID: "helper", Runtime: "exec", Entry: "helper", Start: "lazy", Contract: ServiceContract{ID: "helper", Version: 1}}}
	public, clone := root.Public(), root.Clone()
	service, err := ServiceManifest(root, "helper")
	if err != nil {
		t.Fatal(err)
	}
	if service.Hooks != nil || service.Contract.Has(GatewayHooks) || public.Hooks == nil ||
		!public.Hooks.Request || !public.Hooks.SignIn {
		t.Fatal("hooks were delegated or removed from the primary handshake")
	}
	root.Hooks.Request = false
	root.Hooks.Origins[0] = "gateway-key"
	if !public.Hooks.Request || public.Hooks.Origins[0] != "local" ||
		!clone.Hooks.Request || clone.Hooks.Origins[0] != "local" {
		t.Fatal("hook flags or origins were not deeply frozen")
	}
}

func TestGatewayBrowserOriginRequiresExplicitOptIn(t *testing.T) {
	for _, method := range []string{MethodGatewayRequest, MethodGatewayResponse} {
		for _, explicit := range []bool{false, true} {
			name := "default"
			if explicit {
				name = "explicit"
			}
			t.Run(method+"/"+name, func(t *testing.T) {
				var called atomic.Int32
				module := Module{Manifest: hookManifest(method), Handlers: map[string]Handler{
					method: func(_ context.Context, call Call, _ *Stream) (any, error) {
						called.Add(1)
						return nil, nil
					},
				}}
				if explicit {
					module.Manifest.Hooks.Origins = []string{"browser"}
				}
				if err := module.Validate(); err != nil {
					t.Fatal("browser hook declaration rejected:", err)
				}
				peer := startPeer(t, module)
				configurePeer(t, peer, module.Manifest, "0")
				params := hookParams(method)
				switch value := params.(type) {
				case GatewayRequest:
					value.Context.Origin = "browser"
					params = value
				case GatewayResponse:
					value.Context.Origin = "browser"
					params = value
				}
				peer.send(callFrame("browser", method, params))
				result := peer.read()
				if explicit {
					if result.Error != nil || called.Load() != 1 {
						t.Fatal("explicit browser permission was ignored:", result, called.Load())
					}
				} else if result.Error == nil || result.Error.Code != "permission_denied" || called.Load() != 0 {
					t.Fatal("browser reached a default-private hook:", result, called.Load())
				}
			})
		}
	}
}

func TestToolExposureRequiresAPI12AndPreservesLegacyOmission(t *testing.T) {
	for _, minor := range []int{0, 1, 2} {
		for _, exposure := range []string{"", "shared", "local-owner", "remote", "local"} {
			manifest := testManifest(ToolProvider)
			manifest.Contract.Version.Minor = minor
			manifest.Tools = []ToolDescriptor{{Name: "memory_search", InputSchema: json.RawMessage(`true`), Exposure: exposure}}
			valid := exposure == "" || minor == 2 && (exposure == "shared" || exposure == "local-owner")
			if err := manifest.Validate(); (err == nil) != valid {
				t.Fatalf("minor=%d exposure=%q validation=%v", minor, exposure, err)
			}
			if valid {
				public := manifest.Public()
				if public.Tools[0].Exposure != exposure {
					t.Fatal("exposure disappeared from handshake")
				}
			}
		}
	}
}

func TestGatewayHookRPCUsesTypedBodiesStateAndSanitizedSignIn(t *testing.T) {
	for _, method := range []string{MethodGatewayRequest, MethodGatewayResponse, MethodGatewaySignIn} {
		t.Run(method, func(t *testing.T) {
			peer := hookPeer(t, method, func(ctx context.Context, call Call, stream *Stream) (any, error) {
				if call.Auth != nil {
					t.Fatal("hook received auth")
				}
				switch method {
				case MethodGatewayRequest:
					var request GatewayRequest
					if err := json.Unmarshal(call.Params, &request); err != nil || request.Context != hookContext() {
						t.Fatal("request context changed:", request, err)
					}
					return GatewayRequestResult{Body: request.Body, State: json.RawMessage(`{"source":"recall"}`)}, nil
				case MethodGatewayResponse:
					var response GatewayResponse
					if err := json.Unmarshal(call.Params, &response); err != nil || !response.Success || response.Context != hookContext() {
						t.Fatal("response context changed:", response, err)
					}
				default:
					var notification GatewaySignIn
					if err := json.Unmarshal(call.Params, &notification); err != nil || notification.Event.EndsAt != "2026-11-13T12:00:00Z" {
						t.Fatal("sanitized sign-in changed:", notification, err)
					}
				}
				return json.RawMessage(`{"outcome":"not-started","domain":"memory"}`), nil
			})
			peer.send(callFrame("hook", method, hookParams(method)))
			frame := peer.read()
			if frame.Kind != KindResult || frame.Error != nil {
				t.Fatal("hook RPC failed:", frame)
			}
			if method != MethodGatewayRequest && string(frame.Result) != `{"outcome":"not-started","domain":"memory"}` {
				t.Fatal("provider outcome interpretation changed domain JSON:", frame)
			}
		})
	}
}

func TestGatewayHooksRefuseAuthAndAccountScopesBeforeHandlers(t *testing.T) {
	for _, method := range []string{MethodGatewayRequest, MethodGatewayResponse, MethodGatewaySignIn} {
		for _, field := range []string{"provider", "account", "generation", "auth", "auth-null"} {
			t.Run(method+"/"+field, func(t *testing.T) {
				var called atomic.Int32
				peer := hookPeer(t, method, func(context.Context, Call, *Stream) (any, error) { called.Add(1); return nil, nil })
				frame := callFrame("forbidden", method, hookParams(method))
				switch field {
				case "provider":
					frame.Scope.Provider = "selected"
				case "account":
					frame.Scope.Account = "selected"
				case "generation":
					frame.Scope.Generation = 1
				case "auth":
					frame.Auth = json.RawMessage(`{"token":"private-secret"}`)
				case "auth-null":
					frame.Auth = json.RawMessage(`null`)
				}
				peer.send(frame)
				result := peer.read()
				if result.Error == nil || result.Error.Code != "permission_denied" || called.Load() != 0 ||
					strings.Contains(result.Error.Message, "private-secret") {
					t.Fatal("hook received credential authority:", result, called.Load())
				}
			})
		}
	}
}

func TestGatewayHooksAreUnaryAndWriteReceiptsKeepHostCallbacks(t *testing.T) {
	for _, method := range []string{MethodGatewayRequest, MethodGatewayResponse, MethodGatewaySignIn} {
		for _, event := range []string{"accepted", "data", "progress", "headers"} {
			t.Run(method+"/"+event, func(t *testing.T) {
				effect, found := GatewayMethodEffect(method)
				if !found || effect != map[bool]string{true: "read", false: "write"}[method == MethodGatewayRequest] {
					t.Fatal("gateway effect classification changed")
				}
				peer := hookPeer(t, method, func(ctx context.Context, call Call, stream *Stream) (any, error) {
					var data any = []byte("not a stream")
					if event == "accepted" {
						data = AcceptedEvent{OperationID: call.Scope.Operation}
					}
					_ = stream.Emit(ctx, event, data)
					if method == MethodGatewayRequest {
						return GatewayRequestResult{}, nil
					}
					return json.RawMessage(`{"outcome":"domain-result"}`), nil
				})
				frame := callFrame("hook", method, hookParams(method))
				frame.Scope.Operation, frame.Scope.Attempt = "root-operation", 1
				peer.send(frame)
				result := peer.read()
				if effect == "write" && event == "accepted" {
					if result.Kind != KindEvent || result.Event != "accepted" {
						t.Fatal("write acceptance was refused:", result)
					}
					peer.send(Frame{Kind: KindAck, ID: result.ID, Seq: result.Seq})
					if terminal := peer.read(); terminal.Error != nil || string(terminal.Result) != `{"outcome":"domain-result"}` {
						t.Fatal("write receipt changed terminal JSON:", terminal)
					}
				} else if result.Kind != KindResult || result.Error == nil || result.Error.Code != "invalid_params" {
					t.Fatal("hook emitted a forbidden event or swallowed its error:", result)
				}
			})
		}
	}
	for _, method := range []string{MethodStateGet, MethodStateCAS, MethodNetworkProxy, MethodComponentCall} {
		t.Run("request HostCall "+method, func(t *testing.T) {
			peer := hookPeer(t, MethodGatewayRequest, func(ctx context.Context, call Call, stream *Stream) (any, error) {
				var result json.RawMessage
				err := stream.HostCall(ctx, method, json.RawMessage(`{"value":"scoped"}`), &result)
				return GatewayRequestResult{State: result}, err
			})
			frame := callFrame("parent", MethodGatewayRequest, hookParams(MethodGatewayRequest))
			frame.Scope.Session = "scoped-session"
			peer.send(frame)
			callback := peer.read()
			if callback.Kind != KindHostCall || callback.Method != method || callback.Parent != frame.ID ||
				callback.Scope != frame.Scope || callback.Auth != nil {
				t.Fatal("hook HostCall lost its parent scope:", callback)
			}
			peer.send(Frame{Kind: KindHostResult, ID: callback.ID, Parent: frame.ID, Result: json.RawMessage(`{"stored":true}`)})
			if terminal := peer.read(); terminal.Error != nil || string(terminal.Result) != `{"state":{"stored":true}}` {
				t.Fatal("scoped HostCall result was lost:", terminal)
			}
		})
	}
}

func TestGatewayStateBoundsApplyToRequestResultsAndResponseInputs(t *testing.T) {
	for _, size := range []int{MaxGatewayStateBytes, MaxGatewayStateBytes + 1} {
		state := json.RawMessage(`"` + strings.Repeat("x", size-2) + `"`)
		for _, method := range []string{MethodGatewayRequest, MethodGatewayResponse} {
			t.Run(method+"/"+string(rune(size-MaxGatewayStateBytes+'0')), func(t *testing.T) {
				var called atomic.Int32
				peer := hookPeer(t, method, func(context.Context, Call, *Stream) (any, error) {
					called.Add(1)
					if method == MethodGatewayRequest {
						return GatewayRequestResult{State: state}, nil
					}
					return nil, nil
				})
				var params any = hookParams(method)
				if method == MethodGatewayResponse {
					response := params.(GatewayResponse)
					response.State = state
					params = response
				}
				peer.send(callFrame("bounded", method, params))
				result := peer.read()
				if size <= MaxGatewayStateBytes {
					if result.Error != nil || called.Load() != 1 {
						t.Fatal("exact state bound refused:", result)
					}
				} else if result.Error == nil || result.Error.Code != "resource_exhausted" ||
					method == MethodGatewayResponse && called.Load() != 0 {
					t.Fatal("oversized state accepted or disclosed:", result, called.Load())
				}
			})
		}
	}
}

func TestGatewayPayloadShapeOriginAndRequestResultValidation(t *testing.T) {
	for _, params := range []json.RawMessage{
		json.RawMessage(`{"body":[],"context":{"origin":"local"}}`),
		json.RawMessage(`{"body":{},"context":{"origin":"unknown"}}`),
		json.RawMessage(`{"body":{},"context":{"origin":"local","headers":{"Authorization":"secret"}}}`),
		json.RawMessage(`{"body":{},"context":{"origin":"local","auth":{"token":"secret"}}}`),
	} {
		peer := hookPeer(t, MethodGatewayRequest, testNoop)
		peer.send(callFrame("invalid", MethodGatewayRequest, params))
		if frame := peer.read(); frame.Error == nil || frame.Error.Code != "invalid_params" {
			t.Fatal("invalid hook input accepted:", frame)
		}
	}
	for _, origin := range []string{"gateway-key", "remote", "browser"} {
		peer := hookPeer(t, MethodGatewayRequest, testNoop)
		request := hookParams(MethodGatewayRequest).(GatewayRequest)
		request.Context.Origin = origin
		peer.send(callFrame("private", MethodGatewayRequest, request))
		if frame := peer.read(); frame.Error == nil || frame.Error.Code != "permission_denied" {
			t.Fatal("private default origin policy was ignored:", frame)
		}
	}
	for _, result := range []any{
		json.RawMessage(`[]`), json.RawMessage(`{"body":null}`), json.RawMessage(`{"body":[]}`),
		json.RawMessage(`{"headers":{}}`), json.RawMessage(`{"outcome":"not-started"}`),
	} {
		peer := hookPeer(t, MethodGatewayRequest, func(context.Context, Call, *Stream) (any, error) { return result, nil })
		peer.send(callFrame("invalid", MethodGatewayRequest, hookParams(MethodGatewayRequest)))
		if frame := peer.read(); frame.Error == nil || frame.Error.Code != "invalid_params" {
			t.Fatal("request hook returned an invalid result envelope:", frame)
		}
	}
	for _, payload := range []json.RawMessage{
		json.RawMessage(`{"event":{"provider":"p","state":"ending","endsAt":123}}`),
		json.RawMessage(`{"event":{"provider":"p","state":"ending","endsAt":"tomorrow"}}`),
		json.RawMessage(`{"event":{"provider":"p","state":"ending","token":"secret"}}`),
		json.RawMessage(`{"event":{"provider":"p","state":"unknown"}}`),
	} {
		peer := hookPeer(t, MethodGatewaySignIn, testNoop)
		peer.send(callFrame("invalid", MethodGatewaySignIn, payload))
		if frame := peer.read(); frame.Error == nil || frame.Error.Code != "invalid_params" {
			t.Fatal("non-sanitized sign-in accepted:", frame)
		}
	}
}

func TestGatewayHookCancellationAndFrozenDescriptor(t *testing.T) {
	module := Module{Manifest: hookManifest(MethodGatewayRequest), Handlers: map[string]Handler{
		MethodGatewayRequest: func(ctx context.Context, call Call, stream *Stream) (any, error) {
			err := stream.HostCall(context.Background(), MethodStateGet, json.RawMessage(`{}`), nil)
			return GatewayRequestResult{}, err
		},
	}}
	module.Manifest.Hooks.Origins = []string{"local"}
	peer := startPeer(t, module)
	configurePeer(t, peer, module.Manifest, "0")
	module.Manifest.Hooks.Origins[0] = "remote"
	module.Manifest.Hooks.Request = false
	peer.send(callFrame("hook", MethodGatewayRequest, hookParams(MethodGatewayRequest)))
	callback := peer.read()
	if callback.Kind != KindHostCall {
		t.Fatal("live descriptor was not frozen:", callback)
	}
	peer.send(Frame{Kind: KindCancel, ID: "hook"})
	if frame := peer.read(); frame.Error == nil || frame.Error.Code != "canceled" {
		t.Fatal("hook HostCall did not inherit cancellation:", frame)
	}
	peer.send(Frame{Kind: KindHostResult, ID: callback.ID, Parent: callback.Parent, Result: json.RawMessage(`{}`)})
	peer.send(callFrame("describe", MethodDescribe, nil))
	result := peer.read()
	var public Manifest
	if result.Error != nil || json.Unmarshal(result.Result, &public) != nil || public.Hooks == nil ||
		!public.Hooks.Request || !slices.Equal(public.Hooks.Origins, []string{"local"}) ||
		bytes.Contains(result.Result, []byte(`"auth"`)) {
		t.Fatal("hook handshake changed after cancellation or mutable descriptor edit:", result)
	}
}
