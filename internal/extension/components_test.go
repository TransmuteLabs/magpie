package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func componentFixtureManifest() sdk.Manifest {
	return sdk.Manifest{
		Schema: 1, ID: "composite-fixture", Version: "1.0.0", Runtime: "exec", Entry: "primary",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 1}, Capabilities: []sdk.Capability{sdk.ModuleRPC}},
		Actions: []sdk.ActionDescriptor{
			{ID: "primary.read", Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{ID: "dependency.read", Component: "dependency", Effect: "read", InputSchema: json.RawMessage(`{}`)},
			{ID: "helper.read", Component: "helper", Effect: "read", InputSchema: json.RawMessage(`{}`)},
			{ID: "helper.write", Component: "helper", Effect: "write", InputSchema: json.RawMessage(`{}`)},
		},
		Services: []sdk.ServiceDescriptor{
			{ID: "dependency", Runtime: "exec", Entry: "bin/dependency", Contract: sdk.ServiceContract{ID: "storage", Version: 1},
				Methods: []string{"dependency.read"}, Start: "lazy"},
			{ID: "helper", Runtime: "bun", Entry: "service/helper.ts", Contract: sdk.ServiceContract{ID: "backend", Version: 2},
				Methods: []string{"helper.read", "helper.write"}, Start: "lazy", DependsOn: []string{"dependency"},
				Ready: sdk.ServiceReady{HTTPEndpoints: []string{"ui"}}},
		},
		Window: &sdk.WindowDescriptor{Metadata: "package.json"},
		Bindings: []sdk.BindingDescriptor{
			{ID: "controller.read", From: sdk.PrimaryComponent, Action: "helper.read"},
			{ID: "controller.write", From: sdk.PrimaryComponent, Action: "helper.write"},
			{ID: "page.read", From: "window:page", Action: "helper.read"},
			{ID: "page.service", From: "window:page", Service: "helper", Endpoint: "ui"},
		},
	}
}

type componentInvocation struct {
	component string
	method    string
	scope     sdk.Scope
	auth      json.RawMessage
	request   sdk.ModuleCallRequest
}

type componentFixture struct {
	group      *componentGroup
	primary    *workerClient
	mu         sync.Mutex
	names      map[*workerClient]string
	configured map[*workerClient]sdk.ComponentConfigureRequest
	started    []Installed
	stopped    []string
	calls      []componentInvocation
	ready      func(string, *sdk.ComponentReadyResult)
	action     func(context.Context, componentInvocation, func(Event) error) (json.RawMessage, error)
	stop       func(string)
}

func newComponentFixture(t *testing.T, manager *Manager, entry Installed, config sdk.ConfigSnapshot) *componentFixture {
	t.Helper()
	fixture := &componentFixture{
		group: newComponentGroup(manager, entry, config), primary: &workerClient{},
		names: map[*workerClient]string{}, configured: map[*workerClient]sdk.ComponentConfigureRequest{},
	}
	fixture.names[fixture.primary] = sdk.PrimaryComponent
	fixture.group.start = func(ctx context.Context, root string, entry Installed, bun string,
		callback func(context.Context, sdk.Scope, string, json.RawMessage) (any, error),
	) (*workerClient, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if root != manager.root || entry.Manifest.Component == nil || callback == nil {
			return nil, errors.New("service lost its owner or derived manifest")
		}
		client := &workerClient{}
		fixture.mu.Lock()
		fixture.names[client] = entry.Manifest.Component.ID
		fixture.started = append(fixture.started, entry)
		fixture.mu.Unlock()
		return client, nil
	}
	fixture.group.invoke = func(client *workerClient, ctx context.Context, method string, scope sdk.Scope, auth json.RawMessage,
		params any, consume func(Event) error,
	) (json.RawMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fixture.mu.Lock()
		id := fixture.names[client]
		switch method {
		case sdk.MethodComponentConfigure:
			request, valid := params.(sdk.ComponentConfigureRequest)
			if !valid || request.Component != id || scope != (sdk.Scope{Plugin: entry.Manifest.ID}) || len(auth) != 0 {
				fixture.mu.Unlock()
				return nil, errors.New("invalid component configure authority")
			}
			fixture.configured[client] = request
			fixture.mu.Unlock()
			return json.RawMessage(`{}`), nil
		case sdk.MethodComponentReady:
			request := fixture.configured[client]
			fixture.mu.Unlock()
			result := sdk.ComponentReadyResult{
				Component: id, Contract: request.Contract, Instance: request.Instance,
				ConfigVersion: request.Config.Version, Endpoints: map[string]string{},
			}
			if id == "helper" {
				result.Endpoints["ui"] = "http://127.0.0.1:12345/ui"
			}
			if fixture.ready != nil {
				fixture.ready(id, &result)
			}
			return json.Marshal(result)
		case sdk.MethodComponentStop:
			fixture.calls = append(fixture.calls, componentInvocation{component: id, method: method})
			fixture.mu.Unlock()
			return json.RawMessage(`{}`), nil
		case sdk.MethodModuleCall:
			request, valid := params.(sdk.ModuleCallRequest)
			if !valid {
				fixture.mu.Unlock()
				return nil, errors.New("module RPC did not use its public request")
			}
			record := componentInvocation{component: id, method: method, scope: scope, auth: bytes.Clone(auth), request: request}
			fixture.calls = append(fixture.calls, record)
			fixture.mu.Unlock()
			if fixture.action != nil {
				return fixture.action(ctx, record, consume)
			}
			return json.RawMessage(`{"ok":true}`), nil
		default:
			fixture.mu.Unlock()
			return nil, errors.New("unexpected component method")
		}
	}
	fixture.group.stop = func(client *workerClient) {
		fixture.mu.Lock()
		name := fixture.names[client]
		fixture.stopped = append(fixture.stopped, name)
		fixture.mu.Unlock()
		if fixture.stop != nil {
			fixture.stop(name)
		}
	}
	t.Cleanup(func() {
		if err := fixture.group.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return fixture
}

func simpleComponentFixture(t *testing.T, manifest sdk.Manifest) *componentFixture {
	t.Helper()
	manager := NewManager(t.TempDir())
	fixture := newComponentFixture(t, manager, Installed{Manifest: manifest, Directory: "packages/old-generation", Revision: "old"},
		sdk.ConfigSnapshot{Version: "7", Values: json.RawMessage(`{"label":"old"}`)})
	fixture.group.verify = func(ctx context.Context, entry Installed) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Directory != "packages/old-generation" || entry.Manifest.Version != manifest.Version {
			return errors.New("helper verified another root package generation")
		}
		return nil
	}
	return fixture
}

func TestComponentGroupEagerDependenciesFrozenConfigAndReverseClose(t *testing.T) {
	manifest := componentFixtureManifest()
	manifest.Services[1].Start = "eager"
	fixture := simpleComponentFixture(t, manifest)
	ctx, cancel := context.WithCancel(t.Context())
	if err := fixture.group.prepare(ctx, fixture.primary); err != nil {
		t.Fatal(err)
	}
	cancel()
	if fixture.group.ctx.Err() != nil {
		t.Fatal("the describe/preparation request owned the runtime lifetime")
	}
	if len(fixture.started) != 2 || fixture.started[0].Manifest.Component.ID != "dependency" || fixture.started[1].Manifest.Component.ID != "helper" {
		t.Fatalf("dependency startup order = %+v", fixture.started)
	}
	for _, entry := range fixture.started {
		if entry.Manifest.ID != manifest.ID || entry.Manifest.Version != manifest.Version || len(entry.Manifest.Services) != 0 ||
			entry.Manifest.Component == nil || !entry.Manifest.Contract.Has(sdk.ModuleRPC) {
			t.Fatalf("private service handshake authority = %+v", entry.Manifest)
		}
	}
	manifest.Services[1].Entry = "tampered.ts"
	config := fixture.group.configSnapshot()
	config.Values[2] = 'X'
	if snapshot := fixture.group.configSnapshot(); string(snapshot.Values) != `{"label":"old"}` {
		t.Fatal("configuration snapshot shares caller storage")
	}
	if source := fixture.group.Source(); len(source.Manifest.Services) != 2 || source.Manifest.Services[1].Entry != "service/helper.ts" {
		t.Fatalf("group lost its complete frozen root descriptor: %+v", source.Manifest)
	}
	if err := fixture.group.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.stopped, []string{sdk.PrimaryComponent, "helper", "dependency"}) {
		t.Fatalf("primary-first reverse dependency shutdown = %v", fixture.stopped)
	}
}

func TestComponentGroupPartialStartupCleansOnlyOwnedInstances(t *testing.T) {
	manifest := componentFixtureManifest()
	manifest.Services[1].Start = "eager"
	fixture := simpleComponentFixture(t, manifest)
	start := fixture.group.start
	fixture.group.start = func(ctx context.Context, root string, entry Installed, bun string,
		callback func(context.Context, sdk.Scope, string, json.RawMessage) (any, error),
	) (*workerClient, error) {
		client, err := start(ctx, root, entry, bun, callback)
		if err == nil {
			client.stdin = nopComponentWriter{}
			client.stdout = io.NopCloser(strings.NewReader(""))
		}
		return client, err
	}
	fixture.ready = func(id string, ready *sdk.ComponentReadyResult) {
		if id == "helper" {
			ready.Instance = "wrong-runtime"
		}
	}
	if err := fixture.group.prepare(t.Context(), fixture.primary); err == nil {
		t.Fatal("a helper with the wrong instance was published")
	}
	if err := fixture.group.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.stopped, []string{sdk.PrimaryComponent, "helper", "dependency"}) {
		t.Fatalf("partial startup left resources behind or closed another group: %v", fixture.stopped)
	}
	if len(fixture.calls) != 2 || fixture.calls[0].component != "helper" || fixture.calls[1].component != "dependency" ||
		fixture.calls[0].method != sdk.MethodComponentStop || fixture.calls[1].method != sdk.MethodComponentStop {
		t.Fatalf("configured components lost disposal after readiness failed: %+v", fixture.calls)
	}
}

func TestComponentGroupStartupHonorsCancellationAndReadinessTimeout(t *testing.T) {
	for _, scenario := range []string{"cancellation", "readiness timeout"} {
		t.Run(scenario, func(t *testing.T) {
			manifest := componentFixtureManifest()
			if scenario == "readiness timeout" {
				manifest.Services[1].Ready.TimeoutMs = 20
			}
			fixture := simpleComponentFixture(t, manifest)
			if err := fixture.group.prepare(t.Context(), fixture.primary); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			invoke := fixture.group.invoke
			fixture.group.invoke = func(client *workerClient, ctx context.Context, method string, scope sdk.Scope, auth json.RawMessage,
				params any, consume func(Event) error,
			) (json.RawMessage, error) {
				if method == sdk.MethodComponentReady && fixture.names[client] == "helper" {
					if scenario == "cancellation" {
						cancel()
					}
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return invoke(client, ctx, method, scope, auth, params, consume)
			}
			action, _ := fixture.group.entry.Manifest.ResolveAction("helper.read")
			_, err := fixture.group.actionClient(ctx, action)
			expected := context.Canceled
			if scenario == "readiness timeout" {
				expected = context.DeadlineExceeded
			}
			if !errors.Is(err, expected) {
				t.Fatalf("service startup did not honor %s: %v", scenario, err)
			}
			if err := fixture.group.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fixture.stopped, []string{sdk.PrimaryComponent, "helper", "dependency"}) {
				t.Fatalf("interrupted startup leaked owned components: %v", fixture.stopped)
			}
		})
	}
}

func TestComponentGroupVerificationHonorsStartupContext(t *testing.T) {
	t.Run("default catalog verifier", func(t *testing.T) {
		manager := NewManager(t.TempDir())
		entry := installComponentFixture(t, manager, componentFixtureManifest())
		fixture := newComponentFixture(t, manager, entry, sdk.ConfigSnapshot{Version: "0", Values: json.RawMessage(`{}`)})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := fixture.group.verify(ctx, entry); !errors.Is(err, context.Canceled) {
			t.Fatalf("default catalog verification ignored its canceled context: %v", err)
		}
	})
	for _, scenario := range []string{"caller cancellation", "startup deadline", "group shutdown"} {
		t.Run(scenario, func(t *testing.T) {
			manifest := componentFixtureManifest()
			if scenario == "startup deadline" {
				for index := range manifest.Services {
					manifest.Services[index].Ready.TimeoutMs = 20
				}
			}
			fixture := simpleComponentFixture(t, manifest)
			if err := fixture.group.prepare(t.Context(), fixture.primary); err != nil {
				t.Fatal(err)
			}
			type verificationContextKey struct{}
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), verificationContextKey{}, "parent"))
			defer cancel()
			entered := make(chan struct{})
			fixture.group.verify = func(call context.Context, entry Installed) error {
				if _, bounded := call.Deadline(); !bounded || call.Value(verificationContextKey{}) != "parent" {
					return errors.New("verification lost its startup deadline or parent context")
				}
				close(entered)
				<-call.Done()
				return call.Err()
			}
			result := make(chan error, 1)
			action, _ := fixture.group.entry.Manifest.ResolveAction("helper.read")
			go func() {
				_, err := fixture.group.actionClient(ctx, action)
				result <- err
			}()
			select {
			case <-entered:
			case err := <-result:
				t.Fatalf("service did not verify under its startup context: %v", err)
			}
			switch scenario {
			case "caller cancellation":
				cancel()
			case "group shutdown":
				if err := fixture.group.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			expected := context.Canceled
			if scenario == "startup deadline" {
				expected = context.DeadlineExceeded
			}
			if err := <-result; !errors.Is(err, expected) {
				t.Fatalf("verification ignored %s: %v", scenario, err)
			}
			if err := fixture.group.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(fixture.started) != 0 || !reflect.DeepEqual(fixture.stopped, []string{sdk.PrimaryComponent}) {
				t.Fatalf("interrupted verification started or retained a helper: started=%d stopped=%v", len(fixture.started), fixture.stopped)
			}
		})
	}
}

func TestComponentGroupConfigureDiscoversOnlyOwnedReadyDependencies(t *testing.T) {
	manifest := componentFixtureManifest()
	manifest.Services[0].Ready.HTTPEndpoints = []string{"storage"}
	manifest.Services[1].Start = "eager"
	sibling := manifest.Services[0]
	sibling.ID, sibling.Entry, sibling.Start = "sibling", "bin/sibling", "eager"
	sibling.Methods, sibling.Ready.HTTPEndpoints = nil, nil
	manifest.Services = append(manifest.Services, sibling)
	fixture := simpleComponentFixture(t, manifest)
	fixture.ready = func(id string, ready *sdk.ComponentReadyResult) {
		if id == "dependency" {
			ready.Endpoints["storage"] = "http://127.0.0.1:12346/storage"
		}
	}
	if err := fixture.group.prepare(t.Context(), fixture.primary); err != nil {
		t.Fatal(err)
	}
	requests := map[string]sdk.ComponentConfigureRequest{}
	for client, request := range fixture.configured {
		requests[fixture.names[client]] = request
		if request.Config.Version != "7" || string(request.Config.Values) != `{"label":"old"}` {
			t.Fatalf("component received another configuration generation: %+v", request)
		}
		for name, ready := range request.Services {
			if ready.Component != name || ready.ConfigVersion != request.Config.Version || ready.Instance == "" {
				t.Fatalf("component received another dependency identity/configuration: %+v", request)
			}
		}
	}
	if len(requests["dependency"].Services) != 0 || len(requests["sibling"].Services) != 0 {
		t.Fatal("a helper discovered a service outside its declared dependencies")
	}
	helper := requests["helper"]
	if len(helper.Services) != 1 || helper.Services["dependency"].Endpoints["storage"] != "http://127.0.0.1:12346/storage" {
		t.Fatalf("helper could not discover its ready dependency endpoint: %+v", helper.Services)
	}
	primary := requests[sdk.PrimaryComponent]
	if len(primary.Services) != 2 || primary.Services["helper"].Component != "helper" || primary.Services["sibling"].Component != "sibling" {
		t.Fatalf("primary did not discover its currently ready eager services: %+v", primary.Services)
	}
	helper.Services["dependency"].Endpoints["storage"] = "http://203.0.113.1:12346"
	primary.Services["helper"].Endpoints["ui"] = "http://203.0.113.1:12345"
	delete(primary.Services, "helper")
	dependency, err := fixture.group.readyService(t.Context(), "dependency")
	if err != nil || dependency.Endpoints["storage"] != "http://127.0.0.1:12346/storage" {
		t.Fatal("configure shared its dependency endpoint storage")
	}
	ready, err := fixture.group.readyService(t.Context(), "helper")
	if err != nil || ready.Endpoints["ui"] != "http://127.0.0.1:12345/ui" {
		t.Fatal("primary configure shared its eager service endpoint storage")
	}
}

func TestComponentGroupConcurrentLazyStartAndEndpointOwnership(t *testing.T) {
	fixture := simpleComponentFixture(t, componentFixtureManifest())
	if err := fixture.group.prepareConfig(t.Context(), fixture.primary); err != nil {
		t.Fatal(err)
	}
	if len(fixture.started) != 0 {
		t.Fatal("lazy services started during primary preparation")
	}
	action, _ := fixture.group.entry.Manifest.ResolveAction("helper.read")
	var pending sync.WaitGroup
	results := make(chan error, 10)
	for range cap(results) {
		pending.Add(1)
		go func() {
			defer pending.Done()
			_, err := fixture.group.actionClient(t.Context(), action)
			results <- err
		}()
	}
	pending.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(fixture.started) != 2 {
		t.Fatalf("lazy DAG created %d clients; want 2", len(fixture.started))
	}
	ctx := fixture.group.withAuthority(t.Context(), "window:page", "read")
	endpoint, err := fixture.group.serviceEndpoint(ctx, "window:page", "page.service")
	if err != nil || endpoint != "http://127.0.0.1:12345/ui" {
		t.Fatalf("declared endpoint = %q, %v", endpoint, err)
	}
	ready, err := fixture.group.readyService(t.Context(), "helper")
	if err != nil {
		t.Fatal(err)
	}
	ready.Endpoints["ui"] = "https://unrelated.example"
	persisted, err := fixture.group.readyService(t.Context(), "helper")
	if err != nil || persisted.Endpoints["ui"] != endpoint {
		t.Fatal("service readiness shared a mutable endpoint map")
	}
	if _, err := fixture.group.serviceEndpoint(ctx, sdk.PrimaryComponent, "page.service"); err == nil {
		t.Fatal("another component borrowed the window's endpoint binding")
	}
}

func TestComponentGroupLazyHelperKeepsOldPackageAndConfiguration(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := componentFixtureManifest()
	oldEntry := installComponentFixture(t, manager, manifest)
	oldConfig, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", json.RawMessage(`{"label":"old"}`))
	if err != nil {
		t.Fatal(err)
	}
	old := newComponentFixture(t, manager, oldEntry, oldConfig)
	if err := old.group.prepare(t.Context(), old.primary); err != nil {
		t.Fatal(err)
	}
	manifest.Version = "2.0.0"
	manifest.Services[1].Entry = "service/new-helper.ts"
	newEntry := installComponentFixture(t, manager, manifest)
	newConfig, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "1", json.RawMessage(`{"label":"new"}`))
	if err != nil {
		t.Fatal(err)
	}
	current := newComponentFixture(t, manager, newEntry, newConfig)
	if err := current.group.prepare(t.Context(), current.primary); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []*componentFixture{old, current} {
		action, _ := fixture.group.entry.Manifest.ResolveAction("helper.read")
		if _, err := fixture.group.actionClient(t.Context(), action); err != nil {
			t.Fatal(err)
		}
	}
	if old.started[1].Directory != oldEntry.Directory || old.started[1].Manifest.Entry != "service/helper.ts" ||
		current.started[1].Directory != newEntry.Directory || current.started[1].Manifest.Entry != "service/new-helper.ts" ||
		old.group.config.Version != "1" || current.group.config.Version != "2" {
		t.Fatal("a lazy helper crossed the package/configuration generation boundary")
	}
}

func TestComponentGroupReadinessIdentityEndpointsAndCycles(t *testing.T) {
	for _, scenario := range []string{"component", "contract", "instance", "configVersion", "missing endpoint", "undeclared endpoint", "remote endpoint"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := simpleComponentFixture(t, componentFixtureManifest())
			fixture.ready = func(id string, ready *sdk.ComponentReadyResult) {
				if id != "helper" {
					return
				}
				switch scenario {
				case "component":
					ready.Component = "other"
				case "contract":
					ready.Contract.Version++
				case "instance":
					ready.Instance = "other"
				case "configVersion":
					ready.ConfigVersion = "other"
				case "missing endpoint":
					ready.Endpoints = nil
				case "undeclared endpoint":
					ready.Endpoints = map[string]string{"other": "http://127.0.0.1:12345/ui"}
				case "remote endpoint":
					ready.Endpoints["ui"] = "http://203.0.113.1:12345/ui"
				}
			}
			if err := fixture.group.prepare(t.Context(), fixture.primary); err != nil {
				t.Fatal(err)
			}
			action, _ := fixture.group.entry.Manifest.ResolveAction("helper.read")
			if _, err := fixture.group.actionClient(t.Context(), action); err == nil {
				t.Fatalf("invalid %s readiness was published", scenario)
			}
		})
	}
	manifest := componentFixtureManifest()
	manifest.Services[0].DependsOn = []string{"helper"}
	fixture := simpleComponentFixture(t, manifest)
	if err := fixture.group.prepare(t.Context(), fixture.primary); err == nil || len(fixture.started) != 0 {
		t.Fatal("cyclic services reached runtime startup")
	}
}

func TestComponentGroupBindingScopeAndAcceptedWrite(t *testing.T) {
	fixture := simpleComponentFixture(t, componentFixtureManifest())
	if err := fixture.group.prepare(t.Context(), fixture.primary); err != nil {
		t.Fatal(err)
	}
	store := fixture.group.manager.store
	account, err := store.SaveAccount(Account{Plugin: fixture.group.entry.Manifest.ID, Provider: "vendor", ID: "alice", Auth: json.RawMessage(`{"token":"alice"}`)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: account.Plugin, Provider: account.Provider, Account: account.ID, Generation: account.Generation, Session: "session", Operation: "operation", Attempt: 1}
	read := fixture.group.withAuthority(t.Context(), sdk.PrimaryComponent, "read")
	input := json.RawMessage(`{"value":1}`)
	if _, err := fixture.group.callBinding(read, sdk.PrimaryComponent, "controller.read", scope, account.Auth, input); err != nil {
		t.Fatal(err)
	}
	if call := fixture.calls[len(fixture.calls)-1]; call.component != "helper" || call.scope != scope || !bytes.Equal(call.auth, account.Auth) ||
		call.request.Action != "helper.read" || !bytes.Equal(call.request.Input, input) {
		t.Fatalf("delegated helper changed scope or account: %+v", call)
	}
	write := fixture.group.withAuthority(t.Context(), sdk.PrimaryComponent, "write")
	if _, err := fixture.group.callBinding(read, sdk.PrimaryComponent, "controller.write", scope, account.Auth, input); err == nil {
		t.Fatal("read parent escalated to a write")
	}
	if _, err := fixture.group.callBinding(write, sdk.PrimaryComponent, "controller.write", scope, account.Auth, input); err == nil {
		t.Fatal("helper write created its own root dispatch")
	}
	if _, err := store.Journal(scope, "dispatched"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.group.callBinding(write, sdk.PrimaryComponent, "controller.write", scope, account.Auth, input); err == nil {
		t.Fatal("helper write ran before durable acceptance")
	}
	accepted, err := store.Journal(scope, "accepted")
	if err != nil {
		t.Fatal(err)
	}
	fixture.action = func(ctx context.Context, call componentInvocation, consume func(Event) error) (json.RawMessage, error) {
		authority, _ := ctx.Value(componentAuthorityKey{}).(componentAuthority)
		if authority.group != fixture.group || authority.component != "helper" || authority.effect != "write" {
			return nil, errors.New("helper host callback lost its group/effect authority")
		}
		receipt, _ := json.Marshal(sdk.AcceptedEvent{OperationID: scope.Operation})
		return json.RawMessage(`{"done":true}`), consume(Event{Name: "accepted", Data: receipt})
	}
	if _, err := fixture.group.callBinding(write, sdk.PrimaryComponent, "controller.write", scope, account.Auth, input); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Operation(scope.Plugin, scope.Operation)
	if err != nil || persisted != accepted {
		t.Fatalf("helper rewrote or dispatched the root journal: %+v, %v", persisted, err)
	}
	if _, err := fixture.group.callBinding(write, sdk.PrimaryComponent, "controller.write", scope, json.RawMessage(`{"token":"bob"}`), input); err == nil {
		t.Fatal("another account's credentials reached the helper")
	}
	other := newComponentGroup(fixture.group.manager, fixture.group.entry, fixture.group.configSnapshot())
	defer other.Close(t.Context())
	if _, err := fixture.group.callBinding(other.withAuthority(t.Context(), sdk.PrimaryComponent, "write"), sdk.PrimaryComponent, "controller.write", scope, account.Auth, input); err == nil {
		t.Fatal("a different group borrowed this group's binding")
	}
	account.Auth = json.RawMessage(`{"token":"refreshed"}`)
	if _, err := store.SaveAccount(account, account.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.group.callBinding(write, sdk.PrimaryComponent, "controller.write", scope, json.RawMessage(`{"token":"alice"}`), input); err == nil {
		t.Fatal("revoked account generation reached the helper")
	}
}

func TestComponentGroupUnaryAndAcceptanceReceipts(t *testing.T) {
	for _, event := range []Event{
		{Name: "data", Data: json.RawMessage(`"streaming"`)},
		{Name: "progress", Data: json.RawMessage(`{}`)},
		{Name: "accepted", Data: json.RawMessage(`{"operationId":"wrong-root-operation"}`)},
	} {
		t.Run(event.Name, func(t *testing.T) {
			fixture := simpleComponentFixture(t, componentFixtureManifest())
			if err := fixture.group.prepare(t.Context(), fixture.primary); err != nil {
				t.Fatal(err)
			}
			scope := sdk.Scope{Plugin: fixture.group.entry.Manifest.ID, Operation: "root-operation"}
			for _, state := range []string{"dispatched", "accepted"} {
				if _, err := fixture.group.manager.store.Journal(scope, state); err != nil {
					t.Fatal(err)
				}
			}
			fixture.action = func(ctx context.Context, call componentInvocation, consume func(Event) error) (json.RawMessage, error) {
				return json.RawMessage(`{}`), consume(event)
			}
			ctx := fixture.group.withAuthority(t.Context(), sdk.PrimaryComponent, "write")
			if _, err := fixture.group.callBinding(ctx, sdk.PrimaryComponent, "controller.write", scope, nil, json.RawMessage(`{}`)); err == nil {
				t.Fatalf("module RPC accepted forbidden event %q", event.Name)
			}
		})
	}
}

func TestComponentGroupBindingTerminalResultHostBoundary(t *testing.T) {
	fixture := simpleComponentFixture(t, componentFixtureManifest())
	if err := fixture.group.prepare(t.Context(), fixture.primary); err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: fixture.group.entry.Manifest.ID}
	ctx := fixture.group.withAuthority(t.Context(), sdk.PrimaryComponent, "read")
	ctx = context.WithValue(ctx, hostAuthorityKey{}, hostAuthority{manifest: fixture.group.entry.Manifest, method: sdk.MethodModuleCall})
	params, err := json.Marshal(sdk.ComponentCallRequest{Binding: "controller.read", Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{sdk.MaxHostBytes - 1, sdk.MaxHostBytes, sdk.MaxHostBytes + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			payload := json.RawMessage(`"` + strings.Repeat("x", size-2) + `"`)
			fixture.action = func(context.Context, componentInvocation, func(Event) error) (json.RawMessage, error) {
				return payload, nil
			}
			result, err := fixture.group.manager.hostCall(ctx, scope, sdk.MethodComponentCall, params)
			raw, valid := result.(json.RawMessage)
			if size > sdk.MaxHostBytes {
				if err == nil || len(raw) != 0 {
					t.Fatal("oversized nested result did not return a controlled host error")
				}
				return
			}
			if err != nil || !valid || !bytes.Equal(raw, payload) {
				t.Fatalf("nested result at %d bytes was not preserved: %v", size, err)
			}
		})
	}
	fixture.action = func(context.Context, componentInvocation, func(Event) error) (json.RawMessage, error) {
		return json.RawMessage(`null`), nil
	}
	if _, err := fixture.group.manager.hostCall(ctx, scope, sdk.MethodComponentCall, params); err != nil {
		t.Fatalf("controlled result-size error retired the component group: %v", err)
	}
}

func TestComponentGroupBindingRejectsAccountRotatedDuringLazyStart(t *testing.T) {
	for _, effect := range []string{"read", "write"} {
		t.Run(effect, func(t *testing.T) {
			fixture := simpleComponentFixture(t, componentFixtureManifest())
			if err := fixture.group.prepare(t.Context(), fixture.primary); err != nil {
				t.Fatal(err)
			}
			store := fixture.group.manager.store
			account, err := store.SaveAccount(Account{
				Plugin: fixture.group.entry.Manifest.ID, Provider: "vendor", ID: "alice",
				Auth: json.RawMessage(`{"token":"old"}`),
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			scope := sdk.Scope{
				Plugin: account.Plugin, Provider: account.Provider, Account: account.ID,
				Generation: account.Generation, Session: "session", Operation: "root-operation", Attempt: 1,
			}
			if effect == "write" {
				for _, state := range []string{"dispatched", "accepted"} {
					if _, err := store.Journal(scope, state); err != nil {
						t.Fatal(err)
					}
				}
			}
			started, release := make(chan struct{}), make(chan struct{})
			var released sync.Once
			unblock := func() { released.Do(func() { close(release) }) }
			defer unblock()
			start := fixture.group.start
			fixture.group.start = func(ctx context.Context, root string, entry Installed, bun string,
				callback func(context.Context, sdk.Scope, string, json.RawMessage) (any, error),
			) (*workerClient, error) {
				if entry.Manifest.Component.ID == "helper" {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return start(ctx, root, entry, bun, callback)
			}
			result := make(chan error, 1)
			ctx := fixture.group.withAuthority(t.Context(), sdk.PrimaryComponent, effect)
			go func() {
				_, err := fixture.group.callBinding(ctx, sdk.PrimaryComponent, "controller."+effect, scope, account.Auth, json.RawMessage(`{}`))
				result <- err
			}()
			select {
			case <-started:
			case err := <-result:
				t.Fatalf("binding did not reach lazy startup: %v", err)
			}
			replacement := account
			replacement.Auth = json.RawMessage(`{"token":"new"}`)
			if _, err := store.SaveAccount(replacement, account.Generation); err != nil {
				t.Fatal(err)
			}
			unblock()
			if err := <-result; err == nil {
				t.Fatal("revoked account generation reached the helper after lazy startup")
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if len(fixture.calls) != 0 {
				t.Fatalf("revoked account sent module payloads: %+v", fixture.calls)
			}
			if len(fixture.configured) != 3 {
				t.Fatal("test did not complete root-scoped configuration of the lazy dependency and helper")
			}
		})
	}
}

func TestComponentGroupStopControlAndBoundedClose(t *testing.T) {
	fixture := simpleComponentFixture(t, componentFixtureManifest())
	fixture.primary.stdin = nopComponentWriter{}
	fixture.primary.stdout = io.NopCloser(strings.NewReader(""))
	if err := fixture.group.prepare(t.Context(), fixture.primary); err != nil {
		t.Fatal(err)
	}
	if err := fixture.group.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(fixture.calls) != 1 || fixture.calls[0].method != sdk.MethodComponentStop ||
		!reflect.DeepEqual(fixture.stopped, []string{sdk.PrimaryComponent}) {
		t.Fatalf("healthy primary did not dispose before transport close: calls=%+v close=%v", fixture.calls, fixture.stopped)
	}
	blocked := simpleComponentFixture(t, componentFixtureManifest())
	started, release := make(chan struct{}), make(chan struct{})
	blocked.stop = func(string) { close(started); <-release }
	if err := blocked.group.prepare(t.Context(), blocked.primary); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := blocked.group.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("close ignored the caller's budget: %v", err)
	}
	<-started
	close(release)
	if err := blocked.group.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestComponentGroupPrimarySDKDisposerCanReachLiveHelper(t *testing.T) {
	manifest := sdk.Manifest{
		Schema: 1, ID: "dispose-fixture", Version: "1.0.0", Runtime: "exec", Entry: "primary",
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 1}, Capabilities: []sdk.Capability{sdk.ModuleRPC}},
		Services: []sdk.ServiceDescriptor{{
			ID: "dependency", Runtime: "exec", Entry: "dependency", Start: "eager",
			Contract: sdk.ServiceContract{ID: "dependency", Version: 1},
			Ready:    sdk.ServiceReady{HTTPEndpoints: []string{"health"}},
		}},
	}
	group := newComponentGroup(NewManager(t.TempDir()), Installed{Manifest: manifest},
		sdk.ConfigSnapshot{Version: "0", Values: json.RawMessage(`{}`)})
	group.verify = func(ctx context.Context, _ Installed) error { return ctx.Err() }
	type peer struct {
		connection net.Conn
		close      func()
	}
	peers := map[*workerClient]peer{}
	connect := func(module sdk.Module) *workerClient {
		t.Helper()
		if err := module.Validate(); err != nil {
			t.Fatal(err)
		}
		connection, remote := net.Pipe()
		lifetime, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer remote.Close()
			_ = sdk.Serve(lifetime, remote, remote, module)
		}()
		client := &workerClient{stdin: connection, stdout: connection}
		var once sync.Once
		peers[client] = peer{connection: connection, close: func() {
			once.Do(func() {
				cancel()
				connection.Close()
				<-done
			})
		}}
		return client
	}
	t.Cleanup(func() {
		if err := group.Close(context.Background()); err != nil {
			t.Error(err)
		}
		for _, peer := range peers {
			peer.close()
		}
	})
	var sequence int
	group.invoke = func(client *workerClient, ctx context.Context, method string, scope sdk.Scope, auth json.RawMessage,
		params any, _ func(Event) error,
	) (json.RawMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		connection := peers[client].connection
		deadline, bounded := ctx.Deadline()
		if !bounded {
			return nil, errors.New("component control call has no deadline")
		}
		if err := connection.SetDeadline(deadline); err != nil {
			return nil, err
		}
		body, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		sequence++
		frame := sdk.Frame{Kind: sdk.KindCall, ID: strconv.Itoa(sequence), Method: method, Scope: scope, Auth: auth,
			Params: body, Deadline: deadline.UnixMilli()}
		if err := json.NewEncoder(connection).Encode(frame); err != nil {
			return nil, err
		}
		var result sdk.Frame
		if err := json.NewDecoder(connection).Decode(&result); err != nil {
			return nil, err
		}
		if result.Kind != sdk.KindResult || result.ID != frame.ID {
			return nil, errors.New("unexpected SDK control response")
		}
		if result.Error != nil {
			return nil, result.Error
		}
		return result.Result, nil
	}
	group.stop = func(client *workerClient) { peers[client].close() }
	group.start = func(_ context.Context, _ string, entry Installed, _ string,
		_ func(context.Context, sdk.Scope, string, json.RawMessage) (any, error),
	) (*workerClient, error) {
		return connect(sdk.Module{Manifest: entry.Manifest, Start: func(ctx context.Context, _ sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if ctx.Err() != nil {
					http.Error(w, "helper stopped", http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			return sdk.ModuleReady{Endpoints: map[string]string{"health": server.URL}}, server.Close, nil
		}}), nil
	}
	disposed := make(chan error, 1)
	primary := connect(sdk.Module{Manifest: manifest, Start: func(_ context.Context, module sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
		endpoint := module.Services["dependency"].Endpoints["health"]
		if endpoint == "" {
			return sdk.ModuleReady{}, nil, errors.New("primary did not receive its eager helper endpoint")
		}
		return sdk.ModuleReady{}, func() {
			client := &http.Client{Timeout: componentCloseTimeout / 2}
			response, err := client.Get(endpoint)
			if err == nil {
				response.Body.Close()
				if response.StatusCode != http.StatusOK {
					err = errors.New("helper lifetime ended before primary disposal")
				}
			}
			disposed <- err
		}, nil
	}})
	if err := group.prepare(t.Context(), primary); err != nil {
		t.Fatal(err)
	}
	if err := group.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-disposed:
		if err != nil {
			t.Fatalf("primary SDK disposer could not reach its live helper: %v", err)
		}
	default:
		t.Fatal("primary SDK disposer was not called")
	}
}

type nopComponentWriter struct{}

func (nopComponentWriter) Write(data []byte) (int, error) { return len(data), nil }
func (nopComponentWriter) Close() error                   { return nil }
