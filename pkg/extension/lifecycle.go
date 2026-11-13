package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

type moduleContextKey struct{}

// ModuleContextFrom copies configuration and dependencies while retaining the
// instance's opaque State value. The author synchronizes shared state.
func ModuleContextFrom(ctx context.Context) (ModuleContext, bool) {
	if ctx == nil {
		return ModuleContext{}, false
	}
	value, found := ctx.Value(moduleContextKey{}).(ModuleContext)
	value.Config.Values = bytes.Clone(value.Config.Values)
	value.Services = cloneServices(value.Services)
	return value, found
}

type componentLifecycle struct {
	ctx      context.Context
	manifest Manifest
	start    StartFunc
	mu       sync.Mutex
	request  *ComponentConfigureRequest
	done     chan struct{}
	result   ComponentReadyResult
	err      error
	cancel   context.CancelFunc
	active   context.Context
	dispose  func()
	state    any
	closed   bool
	stopped  chan struct{}
}

func (lifetime *componentLifecycle) configure(ctx context.Context, params json.RawMessage) (any, error) {
	var request ComponentConfigureRequest
	if decodeParams(params, &request) != nil {
		return nil, &Error{Code: "invalid_params", Message: "invalid component configuration"}
	}
	expected := ServiceContract{}
	if lifetime.manifest.Component != nil {
		expected = lifetime.manifest.Component.Contract
	}
	version, err := strconv.ParseUint(request.Config.Version, 10, 64)
	if request.Component != componentID(lifetime.manifest) || request.Contract != expected ||
		request.Instance == "" || len(request.Instance) > 256 ||
		strings.ContainsFunc(request.Instance, func(char rune) bool { return unicode.IsSpace(char) || unicode.IsControl(char) }) ||
		err != nil || strconv.FormatUint(version, 10) != request.Config.Version ||
		len(request.Config.Values) > MaxHostBytes || !json.Valid(request.Config.Values) {
		return nil, &Error{Code: "invalid_params", Message: "component identity or configuration snapshot is invalid"}
	}
	request.Config.Values = bytes.Clone(request.Config.Values)
	if len(request.Services) > 1024 {
		return nil, &Error{Code: "invalid_params", Message: "too many configured component dependencies"}
	}
	for id, ready := range request.Services {
		if !validID(id) || ready.Component != id || !validServiceContract(ready.Contract) ||
			ready.ConfigVersion != request.Config.Version || ready.Instance == "" || len(ready.Instance) > 256 ||
			strings.ContainsFunc(ready.Instance, func(char rune) bool { return unicode.IsSpace(char) || unicode.IsControl(char) }) ||
			len(ready.Endpoints) > 128 {
			return nil, &Error{Code: "invalid_params", Message: "invalid configured component dependency"}
		}
		for name, endpoint := range ready.Endpoints {
			if !validID(name) || !validReadyURL(endpoint) {
				return nil, &Error{Code: "invalid_params", Message: "invalid configured dependency endpoint"}
			}
		}
	}
	request.Services = cloneServices(request.Services)
	lifetime.mu.Lock()
	if lifetime.closed {
		lifetime.mu.Unlock()
		return nil, context.Canceled
	}
	if previous := lifetime.request; previous != nil {
		if previous.Component != request.Component || previous.Contract != request.Contract ||
			previous.Instance != request.Instance || previous.Config.Version != request.Config.Version ||
			!bytes.Equal(previous.Config.Values, request.Config.Values) || !sameServices(previous.Services, request.Services) {
			lifetime.mu.Unlock()
			return nil, &Error{Code: "conflict", Message: "component configuration is immutable for this instance"}
		}
	} else {
		lifetime.request = &request
		lifetime.done = make(chan struct{})
		startCtx, cancel := context.WithCancel(lifetime.ctx)
		lifetime.cancel = cancel
		lifetime.active = startCtx
		stopParent := context.AfterFunc(ctx, cancel)
		go lifetime.initialize(startCtx, ctx, stopParent, request)
	}
	done := lifetime.done
	lifetime.mu.Unlock()
	select {
	case <-done:
		return lifetime.ready()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-lifetime.ctx.Done():
		return nil, lifetime.ctx.Err()
	}
}

func (lifetime *componentLifecycle) initialize(ctx, parent context.Context, stopParent func() bool, request ComponentConfigureRequest) {
	value := ModuleContext{
		Config:    ConfigSnapshot{Version: request.Config.Version, Values: bytes.Clone(request.Config.Values)},
		Component: request.Component, Instance: request.Instance,
		Services: cloneServices(request.Services),
	}
	ready, dispose, err := invokeStart(lifetime.start, ctx, value)
	stopParent()
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if parent.Err() != nil {
		err = parent.Err()
	}
	if err == nil {
		if len(ready.Endpoints) > 128 {
			err = &Error{Code: "invalid_params", Message: "too many component readiness endpoints"}
		}
		for name, endpoint := range ready.Endpoints {
			if !validID(name) || !validReadyURL(endpoint) {
				err = &Error{Code: "invalid_params", Message: "invalid component readiness endpoint"}
				break
			}
		}
	}
	if err != nil {
		lifetime.cancel()
		disposeSafely(dispose)
		dispose = nil
	}
	lifetime.mu.Lock()
	if lifetime.closed && err == nil {
		err = context.Canceled
	}
	lifetime.err = err
	if err == nil {
		lifetime.result = ComponentReadyResult{
			Component: request.Component, Contract: request.Contract, Instance: request.Instance,
			ConfigVersion: request.Config.Version, Endpoints: maps.Clone(ready.Endpoints),
		}
		if lifetime.result.Endpoints == nil {
			lifetime.result.Endpoints = map[string]string{}
		}
		lifetime.dispose = dispose
		lifetime.state = ready.State
		dispose = nil
	}
	lifetime.mu.Unlock()
	disposeSafely(dispose)
	close(lifetime.done)
}

func invokeStart(start StartFunc, ctx context.Context, value ModuleContext) (ready ModuleReady, dispose func(), err error) {
	defer func() {
		if recover() != nil {
			err = &Error{Code: "internal", Message: "component startup failed"}
		}
	}()
	if start != nil {
		return start(ctx, value)
	}
	return ModuleReady{}, nil, nil
}

func (lifetime *componentLifecycle) ready() (ComponentReadyResult, error) {
	lifetime.mu.Lock()
	defer lifetime.mu.Unlock()
	if lifetime.closed {
		return ComponentReadyResult{}, context.Canceled
	}
	if lifetime.done == nil {
		return ComponentReadyResult{}, &Error{Code: "not_ready", Message: "component has not been configured"}
	}
	select {
	case <-lifetime.done:
	default:
		return ComponentReadyResult{}, &Error{Code: "not_ready", Message: "component startup has not completed"}
	}
	if lifetime.err != nil {
		return ComponentReadyResult{}, lifetime.err
	}
	result := lifetime.result
	result.Endpoints = maps.Clone(result.Endpoints)
	return result, nil
}

func (lifetime *componentLifecycle) callContext(ctx context.Context, cancel context.CancelFunc) (context.Context, func(), error) {
	if _, err := lifetime.ready(); err != nil {
		return nil, nil, err
	}
	lifetime.mu.Lock()
	request := *lifetime.request
	active := lifetime.active
	state := lifetime.state
	lifetime.mu.Unlock()
	stop := context.AfterFunc(active, cancel)
	if active.Err() != nil {
		cancel()
	}
	value := ModuleContext{
		Config:    ConfigSnapshot{Version: request.Config.Version, Values: bytes.Clone(request.Config.Values)},
		Component: request.Component, Instance: request.Instance,
		Services: cloneServices(request.Services),
		State:    state,
	}
	return context.WithValue(ctx, moduleContextKey{}, value), func() { stop() }, nil
}

func (lifetime *componentLifecycle) beginStop() <-chan struct{} {
	lifetime.mu.Lock()
	if lifetime.stopped != nil {
		done := lifetime.stopped
		lifetime.mu.Unlock()
		return done
	}
	lifetime.closed = true
	cancel, dispose := lifetime.cancel, lifetime.dispose
	lifetime.dispose = nil
	lifetime.stopped = make(chan struct{})
	done := lifetime.stopped
	lifetime.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if dispose == nil {
		close(done)
	} else {
		go func() {
			defer close(done)
			disposeSafely(dispose)
		}()
	}
	return done
}

func (lifetime *componentLifecycle) close() {
	<-lifetime.beginStop()
}

func (lifetime *componentLifecycle) stop(ctx context.Context) (any, error) {
	select {
	case <-lifetime.beginStop():
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	lifetime.mu.Lock()
	done := lifetime.done
	lifetime.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return struct{}{}, nil
}

func disposeSafely(dispose func()) {
	defer func() { recover() }()
	if dispose != nil {
		dispose()
	}
}

func cloneServices(services map[string]ComponentReadyResult) map[string]ComponentReadyResult {
	if services == nil {
		return nil
	}
	snapshot := make(map[string]ComponentReadyResult, len(services))
	for id, ready := range services {
		ready.Endpoints = maps.Clone(ready.Endpoints)
		snapshot[id] = ready
	}
	return snapshot
}

func sameServices(left, right map[string]ComponentReadyResult) bool {
	return maps.EqualFunc(left, right, func(a, b ComponentReadyResult) bool {
		return a.Component == b.Component && a.Contract == b.Contract && a.Instance == b.Instance &&
			a.ConfigVersion == b.ConfigVersion && maps.Equal(a.Endpoints, b.Endpoints)
	})
}
