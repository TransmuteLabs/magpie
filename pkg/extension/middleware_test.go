package extension

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func middlewareManifest() Manifest {
	manifest := testManifest(GatewayMiddleware)
	manifest.Contract.Version.Minor = 8
	manifest.Middleware = &GatewayMiddlewareDescriptor{Request: true, Account: true, Provider: true, Event: true, Response: true}
	return manifest
}

func TestMiddlewareExplicitRootContract(test *testing.T) {
	root := middlewareManifest()
	if err := root.Validate(); err != nil {
		test.Fatal(err)
	}
	for _, mutate := range []func(*Manifest){
		func(value *Manifest) { value.Contract.Version.Minor = 7 },
		func(value *Manifest) { value.Middleware = nil },
		func(value *Manifest) { value.Contract.Capabilities = []Capability{ModuleRPC} },
		func(value *Manifest) { value.Middleware = &GatewayMiddlewareDescriptor{} },
		func(value *Manifest) { value.Middleware.Origins = []string{"unknown"} },
		func(value *Manifest) { value.Middleware.Origins = []string{"local", "local"} },
		func(value *Manifest) { value.Middleware.Events = []string{"delta\n"} },
		func(value *Manifest) { value.Middleware.Events = []string{"delta", "delta"} },
		func(value *Manifest) { value.Middleware.Event = false; value.Middleware.Events = []string{"delta"} },
		func(value *Manifest) { value.Middleware.Events = make([]string, 65) },
	} {
		value := root.Clone()
		mutate(&value)
		if err := value.Validate(); err == nil {
			test.Fatal("invalid middleware registration gained ordinary request authority")
		}
	}
	if root.Hooks != nil || root.Contract.Has(GatewayHooks) {
		test.Fatal("synchronous middleware inherited asynchronous hook policy")
	}
	root.Contract.Capabilities = append(root.Contract.Capabilities, ModuleRPC)
	root.Middleware.Events, root.Middleware.Origins = []string{"delta"}, []string{"local"}
	root.Services = []ServiceDescriptor{{ID: "helper", Runtime: "exec", Entry: "worker",
		Contract: ServiceContract{ID: "fixture.helper", Version: 1}, Start: "lazy"}}
	service, err := ServiceManifest(root, "helper")
	if err != nil || service.Middleware != nil || service.Contract.Has(GatewayMiddleware) {
		test.Fatal("private service inherited middleware authority:", err)
	}
	public := root.Public()
	root.Middleware.Events[0], root.Middleware.Origins[0] = "changed", "remote"
	if public.Middleware.Events[0] != "delta" || public.Middleware.Origins[0] != "local" {
		test.Fatal("public middleware descriptor retained mutable root storage")
	}
	for _, method := range middlewareManifest().Middleware.Methods() {
		capability, found := MethodCapability(method)
		if !found || capability != GatewayMiddleware {
			test.Fatal("middleware capability registry lost method", method)
		}
	}
}

func TestMiddlewareDTOAuthorityAndBounds(test *testing.T) {
	descriptor := *middlewareManifest().Middleware
	request := MiddlewareCall{Body: json.RawMessage(`{"model":"allowed","stream":true}`),
		Context: MiddlewareContext{GatewayContext: GatewayContext{Origin: "local", Protocol: "chat"}}}
	raw, _ := json.Marshal(request)
	if _, err := DecodeMiddlewareCall(descriptor, MethodMiddlewareRequest, Scope{}, raw); err != nil {
		test.Fatal(err)
	}
	for _, scope := range []Scope{
		{Provider: "foreign"}, {Account: "secret", Generation: 1}, {Session: "foreign"},
		{Operation: "write"}, {Attempt: 1},
	} {
		if _, err := DecodeMiddlewareCall(descriptor, MethodMiddlewareRequest, scope, raw); err == nil {
			test.Fatal("middleware borrowed another authority", scope)
		}
	}
	remote := request
	remote.Context.Origin = "remote"
	remoteRaw, _ := json.Marshal(remote)
	if _, err := DecodeMiddlewareCall(descriptor, MethodMiddlewareRequest, Scope{}, remoteRaw); err == nil {
		test.Fatal("default middleware disclosed a remote request")
	}
	for _, result := range []MiddlewareResult{
		{Body: json.RawMessage(`null`)}, {Body: json.RawMessage(`{"large":"` + strings.Repeat("x", MaxMiddlewareBodyBytes) + `"}`)},
		{State: json.RawMessage(`"` + strings.Repeat("x", MaxGatewayStateBytes) + `"`)},
		{Skip: true}, {Drop: true}, {Reject: &MiddlewareRejection{Status: 200, Message: "invalid"}},
		{Reject: &MiddlewareRejection{Status: 403, Message: "bad\nmessage"}},
		{Reject: &MiddlewareRejection{Status: 403, Message: "denied"}, Body: json.RawMessage(`{}`)},
	} {
		encoded, _ := json.Marshal(result)
		if _, err := DecodeMiddlewareResult(MethodMiddlewareRequest, encoded); err == nil {
			test.Fatal("middleware result exceeded declared authority or bounds")
		}
	}
	if _, err := DecodeMiddlewareResult(MethodMiddlewareEvent, json.RawMessage(`{"reject":{"status":403,"message":"denied"}}`)); err == nil {
		test.Fatal("event middleware acquired request rejection")
	}
	for method, result := range map[string]string{
		MethodMiddlewareRequest: `{"reject":{"status":403,"message":"denied"}}`,
		MethodMiddlewareAccount: `{"skip":true}`, MethodMiddlewareEvent: `{"drop":true}`,
		MethodMiddlewareProvider: `{}`, MethodMiddlewareResponse: `{"body":{}}`,
	} {
		if _, err := DecodeMiddlewareResult(method, json.RawMessage(result)); err != nil {
			test.Fatal(method, err)
		}
	}
	calls := 0
	module := Module{Manifest: middlewareManifest(), Handlers: map[string]Handler{}}
	for _, method := range module.Manifest.Middleware.Methods() {
		module.Handlers[method] = func(context.Context, Call, *Stream) (any, error) {
			calls++
			return MiddlewareResult{}, nil
		}
	}
	handlers, _, err := module.prepare()
	if err != nil {
		test.Fatal(err)
	}
	for _, auth := range []json.RawMessage{json.RawMessage(`{"token":"private"}`), json.RawMessage(`{}`)} {
		if _, err := handlers[MethodMiddlewareRequest](test.Context(), Call{Params: raw, Auth: auth}, nil); err == nil {
			test.Fatal("middleware handler received credentials")
		}
	}
	if calls != 0 {
		test.Fatal("rejected authority reached middleware handler")
	}
	methods := RequiredMethods(module.Manifest)
	expected := module.Manifest.Middleware.Methods()
	if len(methods) != 5 || !reflect.DeepEqual(methods, []string{
		expected[1], expected[3], expected[2], expected[0], expected[4],
	}) {
		test.Fatal("middleware required methods do not match declared hooks:", methods)
	}
}

func TestMiddlewareTypedCallMatchesWire(test *testing.T) {
	descriptor := *middlewareManifest().Middleware
	event := MiddlewareCall{Body: json.RawMessage(` { "text": "<hello>" } `), State: json.RawMessage(`{"seen":true}`),
		Context: MiddlewareContext{GatewayContext: GatewayContext{Origin: "local", Protocol: "chat"}, Event: "delta"}}
	account := MiddlewareCall{Context: event.Context}
	account.Context.Account = &MiddlewareAccount{Ref: strings.Repeat("a", 32), Provider: "test", Kind: "example"}
	provider := event
	provider.Context.Provider = &MiddlewareProvider{ID: "test", Name: "Example", Model: "test/model"}

	for _, item := range []struct {
		name   string
		method string
		scope  Scope
		call   MiddlewareCall
		mutate func(*MiddlewareCall)
		code   string
	}{
		{name: "request", method: MethodMiddlewareRequest, call: event},
		{name: "account", method: MethodMiddlewareAccount, call: account},
		{name: "provider", method: MethodMiddlewareProvider, call: provider},
		{name: "event", method: MethodMiddlewareEvent, call: event},
		{name: "response", method: MethodMiddlewareResponse, call: event},
		{name: "undeclared", method: "middleware.unknown", call: event, code: "invalid_params"},
		{name: "scope", method: MethodMiddlewareEvent, scope: Scope{Session: "borrowed"}, call: event, code: "permission_denied"},
		{name: "origin", method: MethodMiddlewareEvent, call: event, code: "permission_denied",
			mutate: func(call *MiddlewareCall) { call.Context.Origin = "remote" }},
		{name: "missing provider", method: MethodMiddlewareProvider, call: event, code: "invalid_params"},
		{name: "account body", method: MethodMiddlewareAccount, call: account, code: "invalid_params",
			mutate: func(call *MiddlewareCall) { call.Body = json.RawMessage(`{}`) }},
		{name: "body shape", method: MethodMiddlewareEvent, call: event, code: "invalid_params",
			mutate: func(call *MiddlewareCall) { call.Body = json.RawMessage(`[]`) }},
		{name: "escaped body", method: MethodMiddlewareEvent, call: event, code: "invalid_params",
			mutate: func(call *MiddlewareCall) {
				call.Body = json.RawMessage(`{"text":"` + strings.Repeat("<", MaxMiddlewareBodyBytes/5) + `"}`)
			}},
		{name: "escaped state", method: MethodMiddlewareEvent, call: event, code: "invalid_params",
			mutate: func(call *MiddlewareCall) {
				call.State = json.RawMessage(`"` + strings.Repeat(">", MaxGatewayStateBytes/5) + `"`)
			}},
		{name: "normalized text", method: MethodMiddlewareEvent, call: event,
			mutate: func(call *MiddlewareCall) { call.Context.Agent = "local\xff" }},
		{name: "normalized query bound", method: MethodMiddlewareEvent, call: event, code: "invalid_params",
			mutate: func(call *MiddlewareCall) { call.Context.Query = strings.Repeat("\xff", 6<<10) }},
	} {
		test.Run(item.name, func(test *testing.T) {
			call := item.call
			if item.mutate != nil {
				item.mutate(&call)
			}
			wire, err := json.Marshal(call)
			if err != nil {
				test.Fatal(err)
			}
			_, wireErr := DecodeMiddlewareCall(descriptor, item.method, item.scope, wire)
			typedErr := ValidateMiddlewareCall(descriptor, item.method, item.scope, call)
			for name, err := range map[string]error{"typed": typedErr, "wire": wireErr} {
				if item.code == "" {
					if err != nil {
						test.Errorf("%s rejected valid call: %v", name, err)
					}
				} else if found, ok := err.(*Error); !ok || found.Code != item.code {
					test.Errorf("%s error = %v, want %s", name, err, item.code)
				}
			}
		})
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"body":{},"context":{"origin":"local"},"ignored":true}`),
		json.RawMessage(`{"body":{},"context":{"origin":"local"}} {}`),
	} {
		if _, err := DecodeMiddlewareCall(descriptor, MethodMiddlewareEvent, Scope{}, raw); err == nil {
			test.Fatal("wire decoder accepted an invalid envelope", string(raw))
		}
	}
}
