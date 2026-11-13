package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	componentStartupTimeout = 10 * time.Second
	componentCloseTimeout   = 3 * time.Second
)

type componentAuthorityKey struct{}

type componentAuthority struct {
	group     *componentGroup
	component string
	effect    string
}

type groupComponent struct {
	client   *workerClient
	ready    sdk.ComponentReadyResult
	instance string
	starting chan struct{}
	err      error
}

type componentGroup struct {
	manager *Manager
	entry   Installed
	config  sdk.ConfigSnapshot
	ctx     context.Context
	cancel  context.CancelFunc
	initErr error

	mu         sync.Mutex
	components map[string]*groupComponent
	order      []string
	preparing  chan struct{}
	prepared   bool
	prepareErr error
	closed     bool
	starts     sync.WaitGroup
	closeOnce  sync.Once
	closeDone  chan struct{}
	closeErr   error

	start  func(context.Context, string, Installed, string, func(context.Context, sdk.Scope, string, json.RawMessage) (any, error)) (*workerClient, error)
	invoke func(*workerClient, context.Context, string, sdk.Scope, json.RawMessage, any, func(Event) error) (json.RawMessage, error)
	stop   func(*workerClient)
	verify func(context.Context, Installed) error
}

func newComponentGroup(manager *Manager, entry Installed, config sdk.ConfigSnapshot) *componentGroup {
	lifetime, cancel := context.WithCancel(context.Background())
	group := &componentGroup{
		manager: manager, config: cloneModuleConfig(config), ctx: lifetime, cancel: cancel,
		components: map[string]*groupComponent{}, closeDone: make(chan struct{}),
		start: startWorker, invoke: (*workerClient).call, stop: (*workerClient).close,
	}
	if manager == nil || manager.catalog == nil || manager.store == nil {
		group.initErr = errors.New("module component group requires its owning manager")
		return group
	}
	group.verify = manager.catalog.verify
	group.entry, group.initErr = catalogClone(entry)
	if group.initErr == nil {
		group.initErr = group.entry.Manifest.Validate()
	}
	if group.config.Version == "" {
		group.config.Version = "0"
	}
	if len(group.config.Values) == 0 {
		group.config.Values = json.RawMessage(`{}`)
	}
	version, err := strconv.ParseUint(group.config.Version, 10, 64)
	if group.initErr == nil && (err != nil || strconv.FormatUint(version, 10) != group.config.Version) {
		group.initErr = errors.New("module component configuration version is invalid")
	}
	if group.initErr == nil {
		group.initErr = moduleConfigValues(group.config.Values)
	}
	return group
}

func (group *componentGroup) configSnapshot() sdk.ConfigSnapshot {
	return cloneModuleConfig(group.config)
}

func (group *componentGroup) Source() Installed {
	entry, _ := catalogClone(group.entry)
	return entry
}

func (group *componentGroup) prepareConfig(ctx context.Context, primary *workerClient) error {
	return group.prepare(ctx, primary)
}

func (group *componentGroup) withAuthority(ctx context.Context, component, effect string) context.Context {
	return context.WithValue(ctx, componentAuthorityKey{}, componentAuthority{
		group: group, component: component, effect: effect,
	})
}

func (group *componentGroup) callContext(ctx context.Context) (context.Context, func()) {
	call, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(group.ctx, cancel)
	if group.ctx.Err() != nil {
		cancel()
	}
	return call, func() { stop(); cancel() }
}

func (group *componentGroup) prepare(ctx context.Context, primary *workerClient) error {
	if ctx == nil || primary == nil {
		return errors.New("module preparation requires its primary client and context")
	}
	group.mu.Lock()
	if group.closed || group.initErr != nil {
		err := group.initErr
		if err == nil {
			err = errors.New("module component group is closed")
		}
		group.mu.Unlock()
		return err
	}
	if previous := group.components[sdk.PrimaryComponent]; previous != nil && previous.client != primary {
		group.mu.Unlock()
		return errors.New("module component group already owns another primary")
	}
	if group.prepared {
		group.mu.Unlock()
		return nil
	}
	if waiting := group.preparing; waiting != nil {
		group.mu.Unlock()
		select {
		case <-waiting:
			group.mu.Lock()
			err := group.prepareErr
			group.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-group.ctx.Done():
			return errors.New("module component group is closed")
		}
	}
	group.components[sdk.PrimaryComponent] = &groupComponent{client: primary}
	group.order = append(group.order, sdk.PrimaryComponent)
	group.preparing = make(chan struct{})
	ready := group.preparing
	group.starts.Add(1)
	group.mu.Unlock()
	if primary.done != nil {
		go func() {
			select {
			case <-primary.done:
				group.beginClose()
			case <-group.ctx.Done():
			}
		}()
	}
	err := group.preparePrimary(ctx, primary)
	group.mu.Lock()
	group.prepared, group.prepareErr = err == nil, err
	close(ready)
	group.preparing = nil
	group.mu.Unlock()
	group.starts.Done()
	if err != nil {
		group.beginClose()
	}
	return err
}

func (group *componentGroup) preparePrimary(ctx context.Context, primary *workerClient) error {
	for _, service := range group.entry.Manifest.Services {
		if service.Start == "eager" {
			if _, err := group.ensureService(ctx, service.ID); err != nil {
				return err
			}
		}
	}
	if group.entry.Manifest.Contract.Version.Minor < 1 {
		return nil
	}
	ready, instance, err := group.configure(ctx, primary, sdk.PrimaryComponent, sdk.ServiceContract{}, nil, componentStartupTimeout)
	group.mu.Lock()
	group.components[sdk.PrimaryComponent].ready = ready
	group.components[sdk.PrimaryComponent].instance = instance
	group.mu.Unlock()
	return err
}

func (group *componentGroup) ensureService(ctx context.Context, id string) (*workerClient, error) {
	if group.initErr != nil {
		return nil, group.initErr
	}
	var descriptor sdk.ServiceDescriptor
	for _, service := range group.entry.Manifest.Services {
		if service.ID == id {
			descriptor = service
			break
		}
	}
	if descriptor.ID == "" {
		return nil, errors.New("module service was not declared")
	}
	for _, dependency := range descriptor.DependsOn {
		if _, err := group.ensureService(ctx, dependency); err != nil {
			return nil, err
		}
	}
	for {
		group.mu.Lock()
		if group.closed {
			group.mu.Unlock()
			return nil, errors.New("module component group is closed")
		}
		component := group.components[id]
		if component != nil {
			if waiting := component.starting; waiting != nil {
				group.mu.Unlock()
				select {
				case <-waiting:
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-group.ctx.Done():
					return nil, errors.New("module component group is closed")
				}
			}
			client, err := component.client, component.err
			group.mu.Unlock()
			return client, err
		}
		component = &groupComponent{starting: make(chan struct{})}
		group.components[id] = component
		group.starts.Add(1)
		group.mu.Unlock()

		client, ready, instance, err := group.startService(ctx, descriptor)
		group.mu.Lock()
		component.client, component.ready, component.instance, component.err = client, ready, instance, err
		if client != nil {
			group.order = append(group.order, id)
		}
		close(component.starting)
		component.starting = nil
		group.mu.Unlock()
		group.starts.Done()
		if client != nil && client.done != nil {
			go func() {
				select {
				case <-client.done:
					group.beginClose()
				case <-group.ctx.Done():
				}
			}()
		}
		return client, err
	}
}

func (group *componentGroup) startService(ctx context.Context, descriptor sdk.ServiceDescriptor) (*workerClient, sdk.ComponentReadyResult, string, error) {
	manifest, err := sdk.ServiceManifest(group.entry.Manifest, descriptor.ID)
	if err != nil {
		return nil, sdk.ComponentReadyResult{}, "", err
	}
	call, cancel := group.callContext(ctx)
	defer cancel()
	timeout := componentStartupTimeout
	if descriptor.Ready.TimeoutMs > 0 {
		timeout = time.Duration(descriptor.Ready.TimeoutMs) * time.Millisecond
	}
	call, stop := context.WithTimeout(call, timeout)
	defer stop()
	if err := group.verify(call, group.entry); err != nil {
		return nil, sdk.ComponentReadyResult{}, "", err
	}
	entry := group.entry
	entry.Manifest = manifest
	client, err := group.start(call, group.manager.root, entry, group.manager.Bun, group.manager.hostCall)
	if err != nil {
		return nil, sdk.ComponentReadyResult{}, "", err
	}
	ready, instance, err := group.configure(call, client, descriptor.ID, descriptor.Contract, descriptor.Ready.HTTPEndpoints, timeout)
	if err != nil {
		return client, sdk.ComponentReadyResult{}, instance, err
	}
	return client, ready, instance, nil
}

func (group *componentGroup) configure(ctx context.Context, client *workerClient, id string, contract sdk.ServiceContract, endpoints []string, timeout time.Duration) (sdk.ComponentReadyResult, string, error) {
	instance, err := randomID()
	if err != nil {
		return sdk.ComponentReadyResult{}, "", err
	}
	call, cancel := group.callContext(ctx)
	defer cancel()
	call, stop := context.WithTimeout(call, timeout)
	defer stop()
	call = group.withAuthority(call, id, "read")
	call = context.WithValue(call, hostAuthorityKey{}, hostAuthority{manifest: group.entry.Manifest, method: sdk.MethodComponentConfigure})
	scope := sdk.Scope{Plugin: group.entry.Manifest.ID}
	events := func(Event) error { return errors.New("component control calls must be unary") }
	request := sdk.ComponentConfigureRequest{
		Component: id, Contract: contract, Instance: instance, Config: group.configSnapshot(),
	}
	request.Services, err = group.configurationServices(id)
	if err != nil {
		return sdk.ComponentReadyResult{}, "", err
	}
	if _, err := group.invoke(client, call, sdk.MethodComponentConfigure, scope, nil, request, events); err != nil {
		return sdk.ComponentReadyResult{}, "", err
	}
	result, err := group.invoke(client, call, sdk.MethodComponentReady, scope, nil, struct{}{}, events)
	if err != nil {
		return sdk.ComponentReadyResult{}, instance, err
	}
	var ready sdk.ComponentReadyResult
	if !decodeHostParams(result, &ready) || ready.Component != id || ready.Contract != contract ||
		ready.Instance != instance || ready.ConfigVersion != group.config.Version || len(ready.Endpoints) != len(endpoints) {
		return ready, instance, errors.New("component readiness does not match its frozen identity and configuration")
	}
	for name, endpoint := range ready.Endpoints {
		if !slices.Contains(endpoints, name) || !componentLoopbackEndpoint(endpoint) {
			return ready, instance, errors.New("component readiness exposed an undeclared or non-loopback endpoint")
		}
	}
	return ready, instance, nil
}

func (group *componentGroup) configurationServices(id string) (map[string]sdk.ComponentReadyResult, error) {
	group.mu.Lock()
	defer group.mu.Unlock()
	if group.closed {
		return nil, errors.New("module component group is closed")
	}
	var dependencies []string
	for _, descriptor := range group.entry.Manifest.Services {
		if id == sdk.PrimaryComponent && descriptor.Start == "eager" {
			dependencies = append(dependencies, descriptor.ID)
		} else if descriptor.ID == id {
			dependencies = descriptor.DependsOn
			break
		}
	}
	services := make(map[string]sdk.ComponentReadyResult, len(dependencies))
	for _, dependency := range dependencies {
		component := group.components[dependency]
		if component == nil || component.starting != nil || component.err != nil || component.ready.ConfigVersion != group.config.Version {
			return nil, errors.New("component dependency is not ready in its configuration generation")
		}
		services[dependency] = cloneComponentReady(component.ready)
	}
	return services, nil
}

func cloneComponentReady(ready sdk.ComponentReadyResult) sdk.ComponentReadyResult {
	endpoints := make(map[string]string, len(ready.Endpoints))
	for name, endpoint := range ready.Endpoints {
		endpoints[name] = endpoint
	}
	ready.Endpoints = endpoints
	return ready
}

func componentLoopbackEndpoint(endpoint string) bool {
	if len(endpoint) == 0 || len(endpoint) > 8192 {
		return false
	}
	location, err := url.Parse(endpoint)
	if err != nil || location.User != nil || location.Opaque != "" || location.RawQuery != "" || location.Fragment != "" ||
		(location.Scheme != "http" && location.Scheme != "https") {
		return false
	}
	address := net.ParseIP(location.Hostname())
	if address == nil || !address.IsLoopback() {
		return false
	}
	port, err := strconv.ParseUint(location.Port(), 10, 16)
	return err == nil && port != 0
}

func (group *componentGroup) actionClient(ctx context.Context, action sdk.ActionDescriptor) (*workerClient, error) {
	group.mu.Lock()
	owned := group.components[sdk.PrimaryComponent] != nil && !group.closed
	group.mu.Unlock()
	if !owned {
		return nil, errors.New("module action requires its owned primary component")
	}
	declared, exists := group.entry.Manifest.ResolveAction(action.ID)
	if !exists || declared.Component != action.Component || declared.Effect != action.Effect ||
		!bytes.Equal(declared.InputSchema, action.InputSchema) {
		return nil, errors.New("module action differs from the frozen descriptor")
	}
	component := action.Component
	if component == "" {
		component = sdk.PrimaryComponent
	}
	if component != sdk.PrimaryComponent {
		client, err := group.ensureService(ctx, component)
		if err != nil {
			group.beginClose()
		}
		return client, err
	}
	group.mu.Lock()
	defer group.mu.Unlock()
	if group.closed || !group.prepared {
		return nil, errors.New("module primary component is not ready")
	}
	return group.components[sdk.PrimaryComponent].client, nil
}

func (group *componentGroup) binding(ctx context.Context, from, id string) (sdk.BindingDescriptor, error) {
	authority, exists := ctx.Value(componentAuthorityKey{}).(componentAuthority)
	if !exists || authority.group != group || authority.component != from {
		return sdk.BindingDescriptor{}, errors.New("component binding requires its frozen parent authority")
	}
	binding, exists := group.entry.Manifest.ResolveBinding(id)
	if !exists || binding.From != from {
		return sdk.BindingDescriptor{}, errors.New("component binding was not granted to its caller")
	}
	return binding, nil
}

func (group *componentGroup) callBinding(ctx context.Context, from, bindingID string, scope sdk.Scope, auth, input json.RawMessage) (json.RawMessage, error) {
	binding, err := group.binding(ctx, from, bindingID)
	if err != nil {
		return nil, err
	}
	action, found := group.entry.Manifest.ResolveAction(binding.Action)
	if !found || binding.Action == "" || scope.Plugin != group.entry.Manifest.ID {
		return nil, errors.New("component binding does not name an owned action")
	}
	if len(input) == 0 || len(input) > sdk.MaxHostBytes || !json.Valid(input) {
		return nil, errors.New("module action input must be bounded JSON")
	}
	current, err := group.manager.authFor(scope)
	if err != nil || !bytes.Equal(current, auth) {
		return nil, errors.New("component binding account authority changed")
	}
	authority, _ := ctx.Value(componentAuthorityKey{}).(componentAuthority)
	if action.Effect == "write" {
		if authority.effect != "write" {
			return nil, errors.New("a read component call cannot delegate a write")
		}
		if err := group.accepted(scope); err != nil {
			return nil, err
		}
	}
	client, err := group.actionClient(ctx, action)
	if err != nil {
		return nil, err
	}
	component := action.Component
	if component == "" {
		component = sdk.PrimaryComponent
	}
	call, cancel := group.callContext(ctx)
	defer cancel()
	call = group.withAuthority(call, component, action.Effect)
	call = context.WithValue(call, hostAuthorityKey{}, hostAuthority{manifest: group.entry.Manifest, method: sdk.MethodModuleCall})
	current, err = group.manager.authFor(scope)
	if err != nil || !bytes.Equal(current, auth) {
		return nil, errors.New("component binding account authority changed")
	}
	result, err := group.invoke(client, call, sdk.MethodModuleCall, scope, bytes.Clone(auth),
		sdk.ModuleCallRequest{Action: action.ID, Input: bytes.Clone(input)}, func(event Event) error {
			if event.Name != "accepted" || action.Effect != "write" {
				return errors.New("module RPC 1.1 is unary and permits only write acceptance")
			}
			var receipt sdk.AcceptedEvent
			if !decodeHostParams(event.Data, &receipt) || scope.Operation == "" || receipt.OperationID != scope.Operation {
				return errors.New("component accepted another root operation")
			}
			return group.accepted(scope)
		})
	if err != nil {
		return nil, err
	}
	// Nested results cross host.result, whose payload budget is smaller than
	// a direct module.call result's frame budget.
	if len(result) > sdk.MaxHostBytes || !json.Valid(result) {
		return nil, errors.New("component binding returned an invalid or oversized terminal JSON value")
	}
	return bytes.Clone(result), nil
}

func (group *componentGroup) accepted(scope sdk.Scope) error {
	if scope.Operation == "" {
		return errors.New("component write requires an accepted root operation")
	}
	operation, err := group.manager.store.Operation(scope.Plugin, scope.Operation)
	if err != nil || operation.Scope != scope || operation.State != "accepted" {
		return errors.New("component write requires the same durable accepted root operation")
	}
	return nil
}

func (group *componentGroup) serviceEndpoint(ctx context.Context, from, bindingID string) (string, error) {
	binding, err := group.binding(ctx, from, bindingID)
	if err != nil || binding.Service == "" || binding.Endpoint == "" {
		return "", errors.New("component binding does not name a service endpoint")
	}
	if _, err := group.ensureService(ctx, binding.Service); err != nil {
		group.beginClose()
		return "", err
	}
	group.mu.Lock()
	defer group.mu.Unlock()
	if group.closed {
		return "", errors.New("module component group is closed")
	}
	endpoint := group.components[binding.Service].ready.Endpoints[binding.Endpoint]
	if !componentLoopbackEndpoint(endpoint) {
		return "", errors.New("declared component endpoint is unavailable")
	}
	return endpoint, nil
}

func (group *componentGroup) readyService(ctx context.Context, id string) (sdk.ComponentReadyResult, error) {
	if _, err := group.ensureService(ctx, id); err != nil {
		group.beginClose()
		return sdk.ComponentReadyResult{}, err
	}
	group.mu.Lock()
	defer group.mu.Unlock()
	if group.closed {
		return sdk.ComponentReadyResult{}, errors.New("module component group is closed")
	}
	return cloneComponentReady(group.components[id].ready), nil
}

func (group *componentGroup) stopComponent(ctx context.Context, component string, client *workerClient) error {
	group.mu.Lock()
	configured := group.components[component].instance != ""
	group.mu.Unlock()
	client.mu.Lock()
	healthy := client.err == nil && len(client.pending) == 0 && client.stdin != nil && client.stdout != nil
	client.mu.Unlock()
	var stopErr error
	if configured && healthy && ctx.Err() == nil && client.alive() {
		control, cancel := context.WithTimeout(ctx, time.Second)
		control = group.withAuthority(control, component, "read")
		control = context.WithValue(control, hostAuthorityKey{}, hostAuthority{manifest: group.entry.Manifest, method: sdk.MethodComponentStop})
		_, stopErr = group.invoke(client, control, sdk.MethodComponentStop, sdk.Scope{Plugin: group.entry.Manifest.ID}, nil, struct{}{},
			func(Event) error { return errors.New("component stop must be unary") })
		cancel()
	}
	group.stop(client)
	return stopErr
}

func (group *componentGroup) beginClose() {
	group.closeOnce.Do(func() {
		group.mu.Lock()
		group.closed = true
		group.mu.Unlock()
		group.cancel()
		go func() {
			defer close(group.closeDone)
			shutdown, cancel := context.WithTimeout(context.Background(), componentCloseTimeout)
			defer cancel()
			group.starts.Wait()
			group.mu.Lock()
			var clients []struct {
				component string
				client    *workerClient
			}
			// The primary may use its ready helpers while disposing. Quiesce
			// it before stopping helpers in reverse dependency order.
			order := []string{sdk.PrimaryComponent}
			for index := len(group.order) - 1; index >= 0; index-- {
				if id := group.order[index]; id != sdk.PrimaryComponent {
					order = append(order, id)
				}
			}
			for _, id := range order {
				if component := group.components[id]; component != nil && component.client != nil {
					clients = append(clients, struct {
						component string
						client    *workerClient
					}{id, component.client})
				}
			}
			group.mu.Unlock()
			for _, owned := range clients {
				if err := group.stopComponent(shutdown, owned.component, owned.client); err != nil {
					group.mu.Lock()
					if group.closeErr == nil {
						group.closeErr = err
					}
					group.mu.Unlock()
				}
			}
		}()
	})
}

func (group *componentGroup) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("component group close requires a context")
	}
	group.beginClose()
	closeContext, cancel := context.WithTimeout(ctx, componentCloseTimeout)
	defer cancel()
	select {
	case <-group.closeDone:
		group.mu.Lock()
		err := group.closeErr
		group.mu.Unlock()
		return err
	case <-closeContext.Done():
		return fmt.Errorf("module components did not close before their deadline: %w", closeContext.Err())
	}
}
