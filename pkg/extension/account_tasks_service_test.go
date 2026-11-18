package extension

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func accountTaskServiceRoot(t *testing.T) Manifest {
	t.Helper()
	manifest := accountTasksManifest(t)
	manifest.Actions[0].Component = "paid"
	manifest.Actions = append(manifest.Actions,
		ActionDescriptor{ID: "normal.write", Component: "paid", Effect: "write", InputSchema: json.RawMessage(`{}`)},
		ActionDescriptor{ID: "normal.read", Component: "paid", Effect: "read", InputSchema: json.RawMessage(`{}`)},
		ActionDescriptor{ID: "plain.write", Component: "plain", Effect: "write", InputSchema: json.RawMessage(`{}`)})
	manifest.Services = []ServiceDescriptor{
		{ID: "paid", Runtime: "exec", Entry: "worker", Contract: ServiceContract{ID: "paid", Version: 1},
			Methods: []string{"paid.ping", "normal.write", "normal.read"}, Start: "lazy"},
		{ID: "plain", Runtime: "exec", Entry: "worker", Contract: ServiceContract{ID: "plain", Version: 1},
			Methods: []string{"plain.write"}, Start: "lazy"},
	}
	return manifest
}

func serviceTaskGrant(t *testing.T, manifest Manifest) []string {
	t.Helper()
	raw, err := json.Marshal(manifest.Component)
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		AccountTaskActions []string `json:"accountTaskActions"`
	}
	if err := json.Unmarshal(raw, &identity); err != nil {
		t.Fatal(err)
	}
	return identity.AccountTaskActions
}

func TestAccountTaskServiceProjectionGrantsOnlyDeclaredWrites(t *testing.T) {
	root := accountTaskServiceRoot(t)
	service, err := ServiceManifest(root, "paid")
	if err != nil {
		t.Fatal(err)
	}
	if grant := serviceTaskGrant(t, service); !slices.Equal(grant, []string{"paid.ping"}) {
		t.Fatal("private paid service lost its exact declared task action grant", grant)
	}
	if service.Contract.Version.Minor != 9 || !slices.Equal(service.Contract.Capabilities, []Capability{ModuleRPC}) ||
		!slices.Equal(RequiredMethods(service), []string{MethodModuleCall}) || len(service.Providers) != 0 {
		t.Fatal("task service gained primary provider/auth/usage authority", service)
	}
	if err := service.Validate(); err != nil {
		t.Fatal(err)
	}
	plain, err := ServiceManifest(root, "plain")
	if err != nil || plain.Contract.Version.Minor != 1 || len(serviceTaskGrant(t, plain)) != 0 ||
		!slices.Equal(plain.Contract.Capabilities, []Capability{ModuleRPC}) {
		t.Fatal("ordinary private service changed its API1.1 contract", plain, err)
	}
	rootCopy := root.Clone()
	rootCopy.Providers[0].AccountTasks[0].Action = "normal.write"
	original, err := ServiceManifest(root, "paid")
	if err != nil || !slices.Equal(serviceTaskGrant(t, original), []string{"paid.ping"}) {
		t.Fatal("root clone changed its original task delegation", original, err)
	}
	for _, copy := range []Manifest{service.Clone(), service.Public()} {
		grant := reflect.ValueOf(copy.Component).Elem().FieldByName("AccountTaskActions")
		if !grant.IsValid() || grant.Len() != 1 {
			t.Fatal("service clone/public projection lost task authority", copy)
		}
		grant.Index(0).SetString("normal.write")
		if !slices.Equal(serviceTaskGrant(t, service), []string{"paid.ping"}) {
			t.Fatal("service clone/public projection retained mutable task authority")
		}
	}
	public := root.Public()
	if len(public.Actions) != 0 || len(public.Providers[0].AccountTasks) != 0 {
		t.Fatal("root public handshake exposed its private paid action", public)
	}
}

func TestAccountTaskServiceRejectsInvalidGrant(t *testing.T) {
	service, err := ServiceManifest(accountTaskServiceRoot(t), "paid")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		minor   uint32
		actions []string
	}{
		{name: "oldminor", minor: 8, actions: []string{"paid.ping"}},
		{name: "read", minor: 9, actions: []string{"normal.read"}},
		{name: "missing", minor: 9, actions: []string{"missing"}},
		{name: "duplicate", minor: 9, actions: []string{"paid.ping", "paid.ping"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, _ := json.Marshal(service)
			var object map[string]any
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			object["component"].(map[string]any)["accountTaskActions"] = test.actions
			object["contract"].(map[string]any)["version"].(map[string]any)["minor"] = test.minor
			raw, _ = json.Marshal(object)
			var invalid Manifest
			if err := json.Unmarshal(raw, &invalid); err != nil {
				t.Fatal(err)
			}
			if err := invalid.Validate(); err == nil {
				t.Fatal("invalid private task action grant was admitted", test.name)
			}
		})
	}
}

func TestAccountTaskServiceUsagePreservesFactsThroughCancellation(t *testing.T) {
	service, err := ServiceManifest(accountTaskServiceRoot(t), "paid")
	if err != nil {
		t.Fatal(err)
	}
	exerciseAccountTaskUsageCancellation(t, service)
}

func TestAccountTaskServiceNormalActionsCannotReportUsage(t *testing.T) {
	for _, private := range []bool{false, true} {
		for _, action := range []string{"normal.write", "normal.read", "missing"} {
			t.Run(action+map[bool]string{false: "/primary", true: "/service"}[private], func(t *testing.T) {
				manifest := accountTasksManifest(t)
				manifest.Actions = append(manifest.Actions,
					ActionDescriptor{ID: "normal.write", Effect: "write", InputSchema: json.RawMessage(`{}`)},
					ActionDescriptor{ID: "normal.read", Effect: "read", InputSchema: json.RawMessage(`{}`)})
				if private {
					var err error
					manifest, err = ServiceManifest(accountTaskServiceRoot(t), "paid")
					if err != nil {
						t.Fatal(err)
					}
				}
				manifest.ID = "fixture"
				handlers := map[string]Handler{}
				for _, method := range RequiredMethods(manifest) {
					if method != MethodModuleCall {
						handlers[method] = testNoop
					}
				}
				actions := map[string]Handler{}
				for _, declared := range manifest.Actions {
					actions[declared.ID] = func(ctx context.Context, _ Call, stream *Stream) (any, error) {
						return nil, stream.ReportAccountTaskUsage(ctx, OwnCall{Kind: "cache_keepalive", Model: "m1", Usage: Usage{InputTokens: 7}})
					}
				}
				peer := startPeer(t, Module{Manifest: manifest, Handlers: handlers, Actions: actions})
				configurePeer(t, peer, manifest, "1")
				peer.send(callFrame("ordinary", MethodModuleCall, ModuleCallRequest{Action: action, Input: json.RawMessage(`{}`)}))
				reply := peer.read()
				if reply.Kind != KindResult || reply.Error == nil {
					t.Fatal("ordinary or undeclared action gained paid task progress authority", reply)
				}
			})
		}
	}
}

func TestAccountTaskServiceGrantDoesNotDelegateOwnCallOrAuth(t *testing.T) {
	manifest, err := ServiceManifest(accountTaskServiceRoot(t), "paid")
	if err != nil {
		t.Fatal(err)
	}
	manifest.ID = "fixture"
	raw := json.RawMessage(`{"outcome":"completed","call":{"kind":"cache_keepalive","model":"m1","usage":{"facts":{"inputTokens":7,"cachedTokens":5}}}}`)
	if _, err := DecodeAccountTaskResult(manifest, raw); err != nil {
		t.Fatal("private task grant rejected its bounded terminal facts", err)
	}
	for _, raw := range []string{
		`{"outcome":"completed","call":{"kind":"cache_keepalive","model":"m1","account":"other","usage":{"inputTokens":7}}}`,
		`{"outcome":"completed","call":{"kind":"cache_keepalive","model":"m1","usage":{"inputTokens":-1}}}`,
		`{"outcome":"completed","call":{"kind":"cache_keepalive","model":"bad model","usage":{}}}`,
	} {
		if _, err := DecodeAccountTaskResult(manifest, json.RawMessage(raw)); err == nil {
			t.Fatal("private task grant admitted arbitrary identity or invalid terminal facts", raw)
		}
	}
	measured := OwnCall{Kind: "cache_keepalive", Model: "m1", Usage: Usage{InputTokens: 7}}
	if err := ValidateOwnCall(manifest, measured); err == nil {
		t.Fatal("private task grant gained normal own-call accounting authority")
	}
	actions := map[string]Handler{}
	for _, action := range manifest.Actions {
		actions[action.ID] = testNoop
	}
	actions["paid.ping"] = func(ctx context.Context, _ Call, stream *Stream) (any, error) {
		return stream.AppendOwnCall(ctx, measured)
	}
	peer := startPeer(t, Module{Manifest: manifest, Actions: actions})
	configurePeer(t, peer, manifest, "1")
	for _, method := range []string{MethodProviderUsage, MethodProviderCalls, MethodAuthRefresh} {
		peer.send(callFrame(method, method, nil))
		if reply := peer.read(); reply.Kind != KindResult || reply.Error == nil || reply.Error.Code != "method_not_found" {
			t.Fatal("private task grant gained provider/history/auth authority", reply)
		}
	}
	peer.send(callFrame("append", MethodModuleCall, ModuleCallRequest{Action: "paid.ping", Input: json.RawMessage(`{}`)}))
	if reply := peer.read(); reply.Kind != KindResult || reply.Error == nil {
		t.Fatal("private task grant gained usage.append authority", reply)
	}
}
