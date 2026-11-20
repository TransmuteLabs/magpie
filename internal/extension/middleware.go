package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sync"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type MiddlewareRun struct {
	Module string

	manager    *Manager
	entry      Installed
	config     string
	descriptor sdk.GatewayMiddlewareDescriptor
	info       sdk.GatewayContext
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	lease      *Lease
	state      json.RawMessage
	closed     bool
}

func (m *Manager) BeginMiddleware(ctx context.Context, info sdk.GatewayContext) []*MiddlewareRun {
	if ctx == nil || ctx.Err() != nil || m.isClosed() {
		return nil
	}
	entries, err := m.catalog.list(ctx)
	if err != nil {
		return nil
	}
	var runs []*MiddlewareRun
	for _, entry := range entries {
		descriptor := entry.Manifest.Middleware
		if entry.Disabled || entry.observerEnablementErr != nil || !entry.Manifest.Contract.Has(sdk.GatewayMiddleware) ||
			descriptor == nil || !descriptor.AllowsOrigin(info.Origin) || len(runs) >= 32 {
			continue
		}
		config, err := m.runtimeModuleConfigContext(ctx, entry.Manifest)
		if err != nil {
			continue
		}
		owned, cancel := context.WithCancel(ctx)
		runs = append(runs, &MiddlewareRun{Module: entry.Manifest.ID, manager: m, entry: entry,
			config: config.Version, descriptor: *entry.Manifest.Clone().Middleware,
			info: info, ctx: owned, cancel: cancel})
	}
	return runs
}

func (run *MiddlewareRun) Descriptor() sdk.GatewayMiddlewareDescriptor {
	descriptor := run.descriptor
	descriptor.Events = slices.Clone(descriptor.Events)
	descriptor.Origins = slices.Clone(descriptor.Origins)
	return descriptor
}

func (run *MiddlewareRun) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if run.manager.isClosed() {
		return errors.New("middleware host is closed")
	}
	entries, err := run.manager.catalog.list(ctx)
	if err != nil {
		return errors.New("middleware catalog is unavailable")
	}
	for _, entry := range entries {
		if entry.Manifest.ID != run.Module || entry.Disabled || entry.Revision != run.entry.Revision ||
			entry.observerEnablementErr != nil || entry.EnableGeneration != run.entry.EnableGeneration ||
			!entry.Manifest.Contract.Has(sdk.GatewayMiddleware) ||
			!reflect.DeepEqual(entry.Manifest.Middleware, &run.descriptor) {
			continue
		}
		config, err := run.manager.runtimeModuleConfigContext(ctx, entry.Manifest)
		if err == nil && config.Version == run.config {
			return nil
		}
	}
	return errors.New("middleware package or configuration changed")
}

func (run *MiddlewareRun) Apply(method string, body []byte, extra sdk.MiddlewareContext) (sdk.MiddlewareResult, error) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.closed || !slices.Contains(run.descriptor.Methods(), method) {
		return sdk.MiddlewareResult{}, errors.New("middleware hook is unavailable")
	}
	if len(body) > sdk.MaxMiddlewareBodyBytes {
		return sdk.MiddlewareResult{}, errors.New("middleware body exceeds its byte limit")
	}
	if err := run.check(run.ctx); err != nil {
		return sdk.MiddlewareResult{}, err
	}
	extra.GatewayContext = run.info
	request := sdk.MiddlewareCall{Body: bytes.Clone(body), State: bytes.Clone(run.state), Context: extra}
	raw, err := json.Marshal(request)
	if err != nil {
		return sdk.MiddlewareResult{}, err
	}
	if _, err := sdk.DecodeMiddlewareCall(run.descriptor, method, sdk.Scope{}, raw); err != nil {
		return sdk.MiddlewareResult{}, err
	}
	if run.lease == nil {
		startup, cancel := context.WithTimeout(run.ctx, time.Second)
		worker, client, err := run.manager.acquire(startup, run.Module, &expectedGeneration{
			packageRevision: run.entry.Revision, configVersion: run.config, enableGeneration: &run.entry.EnableGeneration,
		})
		cancel()
		if err != nil {
			return sdk.MiddlewareResult{}, errors.New("middleware could not start")
		}
		lifetime, cancel := context.WithCancel(run.ctx)
		run.lease = &Lease{manager: run.manager, worker: worker, client: client, group: client.group,
			ctx: lifetime, cancel: cancel}
	}
	limit := 250 * time.Millisecond
	if method == sdk.MethodMiddlewareEvent {
		limit = 50 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(run.ctx, limit)
	defer cancel()
	previous, _ := ctx.Value(windowAdmissionKey{}).(func(context.Context) error)
	ctx = context.WithValue(ctx, windowAdmissionKey{}, func(current context.Context) error {
		if previous != nil {
			if err := previous(current); err != nil {
				return err
			}
		}
		return run.check(current)
	})
	ctx = context.WithValue(ctx, gatewayReadOnlyKey{}, true)
	output, err := run.lease.Call(ctx, method, sdk.Scope{}, request, nil)
	if err != nil {
		return sdk.MiddlewareResult{}, errors.New("middleware hook failed")
	}
	result, err := sdk.DecodeMiddlewareResult(method, output)
	if err != nil {
		return sdk.MiddlewareResult{}, err
	}
	if len(result.Body) > 0 && (method == sdk.MethodMiddlewareRequest || method == sdk.MethodMiddlewareProvider) &&
		!gatewayHookChange(body, result.Body) {
		return sdk.MiddlewareResult{}, errors.New("middleware changed protected request fields")
	}
	if err := run.check(ctx); err != nil {
		return sdk.MiddlewareResult{}, err
	}
	if len(result.State) > 0 {
		run.state = bytes.Clone(result.State)
	}
	return result, nil
}

func (run *MiddlewareRun) Close() {
	run.cancel()
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.closed {
		return
	}
	run.closed = true
	if run.lease != nil {
		run.lease.Close()
	}
	run.state = nil
}
