package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	return m.SelectGateway(ctx, info.Origin).BeginMiddleware(ctx, info)
}

func (selection GatewaySelection) BeginMiddleware(ctx context.Context, info sdk.GatewayContext) []*MiddlewareRun {
	if ctx == nil || ctx.Err() != nil {
		return nil
	}
	info.Origin = selection.origin
	var runs []*MiddlewareRun
	for _, entry := range selection.middleware {
		if len(runs) == 32 {
			break
		}
		descriptor := entry.Manifest.Middleware
		config, err := selection.manager.runtimeModuleConfigContext(ctx, entry)
		if err != nil {
			continue
		}
		owned, cancel := context.WithCancel(ctx)
		runs = append(runs, &MiddlewareRun{Module: entry.Manifest.ID, manager: selection.manager, entry: entry,
			config: config.Version, descriptor: *descriptor,
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

var errMiddlewareChanged = errors.New("middleware package or configuration changed")

func (run *MiddlewareRun) check(ctx context.Context) error {
	if err := run.manager.checkActiveGeneration(ctx, run.entry, run.config); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errMiddlewareChanged
	}
	return nil
}

func (run *MiddlewareRun) Apply(method string, body []byte, extra sdk.MiddlewareContext) (sdk.MiddlewareResult, error) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.closed || !run.descriptor.DeclaresMethod(method) {
		return sdk.MiddlewareResult{}, errors.New("middleware hook is unavailable")
	}
	if len(body) > sdk.MaxMiddlewareBodyBytes {
		return sdk.MiddlewareResult{}, errors.New("middleware body exceeds its byte limit")
	}
	extra.GatewayContext = run.info
	request := sdk.MiddlewareCall{Body: body, State: run.state, Context: extra}
	if err := sdk.ValidateMiddlewareCall(run.descriptor, method, sdk.Scope{}, request); err != nil {
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
	ctx = withContributionAdmission(ctx, run.check)
	var result sdk.MiddlewareResult
	var validationErr error
	_, err := run.lease.call(ctx, method, sdk.Scope{}, request, nil, func(output json.RawMessage) {
		result, validationErr = sdk.DecodeMiddlewareResult(method, output)
		if validationErr == nil && len(result.Body) > 0 &&
			(method == sdk.MethodMiddlewareRequest || method == sdk.MethodMiddlewareProvider) &&
			!gatewayHookChange(body, result.Body) {
			validationErr = errors.New("middleware changed protected request fields")
		}
	})
	if err != nil {
		if errors.Is(err, errMiddlewareChanged) {
			return sdk.MiddlewareResult{}, errMiddlewareChanged
		}
		return sdk.MiddlewareResult{}, errors.New("middleware hook failed")
	}
	if validationErr != nil {
		return sdk.MiddlewareResult{}, validationErr
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
