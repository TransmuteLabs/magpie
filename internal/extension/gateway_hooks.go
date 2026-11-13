package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"sync"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	maxGatewayRuns     = 128
	gatewayRunLifetime = 15 * time.Minute
	gatewayBodyLimit   = 16 << 20
)

type gatewayHookCall struct {
	lease      *Lease
	descriptor sdk.GatewayHookDescriptor
	state      json.RawMessage
}

type gatewayReadOnlyKey struct{}

// GatewayRun pins each participating module until its completion notification
// finishes. Request cancellation ends recall, but client EOF after a complete
// response does not cancel capture.
type GatewayRun struct {
	Body json.RawMessage

	manager *Manager
	info    sdk.GatewayContext
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	stop    func() bool
	expiry  func() bool
	mu      sync.Mutex
	calls   []gatewayHookCall
	closed  bool
	finish  bool
	once    sync.Once
}

func (m *Manager) HasGateway() bool {
	entries, err := m.List()
	return err == nil && slices.ContainsFunc(entries, func(entry Installed) bool {
		return !entry.Disabled && entry.Manifest.Contract.Has(sdk.GatewayHooks) && entry.Manifest.Hooks != nil
	})
}

func gatewayHookTimeout(descriptor sdk.GatewayHookDescriptor) time.Duration {
	if descriptor.TimeoutMs == 0 {
		return time.Second
	}
	return time.Duration(descriptor.TimeoutMs) * time.Millisecond
}

func gatewayHookBudget(entries []Installed) time.Duration {
	var total, longest time.Duration
	for _, entry := range entries {
		duration := gatewayHookTimeout(*entry.Manifest.Hooks)
		total += duration
		longest = max(longest, duration)
	}
	return max(longest, min(10*time.Second, max(2*time.Second, total)))
}

func gatewayCaptureBudget(calls []gatewayHookCall) time.Duration {
	var total, longest time.Duration
	for _, call := range calls {
		if !call.descriptor.Response {
			continue
		}
		duration := gatewayHookTimeout(call.descriptor)
		total += duration
		longest = max(longest, duration)
	}
	return max(longest, min(10*time.Second, max(2*time.Second, total)))
}

func gatewayHookOrigin(descriptor sdk.GatewayHookDescriptor, origin string) bool {
	return descriptor.AllowsOrigin(origin)
}

func (m *Manager) gatewayEntries(origin string, signin bool) []Installed {
	entries, err := m.List()
	if err != nil {
		return nil
	}
	return slices.DeleteFunc(entries, func(entry Installed) bool {
		hooks := entry.Manifest.Hooks
		return entry.Disabled || !entry.Manifest.Contract.Has(sdk.GatewayHooks) || hooks == nil ||
			!gatewayHookOrigin(*hooks, origin) ||
			signin && !hooks.SignIn || !signin && !hooks.Request && !hooks.Response
	})
}

func (m *Manager) openGatewayRun(ctx context.Context, body []byte, info sdk.GatewayContext, lifetime time.Duration) *GatewayRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || len(m.gatewayRuns) >= maxGatewayRuns {
		return nil
	}
	m.startLifecycleLocked()
	owned, cancel := context.WithTimeout(context.WithoutCancel(ctx), lifetime)
	run := &GatewayRun{
		manager: m, info: info, Body: bytes.Clone(body),
		ctx: owned, cancel: cancel, done: make(chan struct{}),
	}
	if m.gatewayRuns == nil {
		m.gatewayRuns = make(map[*GatewayRun]struct{})
	}
	m.gatewayRuns[run] = struct{}{}
	run.stop = context.AfterFunc(m.startCtx, run.abort)
	run.expiry = context.AfterFunc(owned, run.abort)
	return run
}

// acquireGatewayLease uses the request's bounded startup context without
// making the resulting lease depend on the client's connection lifetime.
func (m *Manager) acquireGatewayLease(ctx, lifetime context.Context, entry Installed) (*Lease, error) {
	config, err := m.runtimeModuleConfig(entry.Manifest)
	if err != nil {
		return nil, err
	}
	worker, client, err := m.acquire(ctx, entry.Manifest.ID, &expectedGeneration{
		packageRevision: entry.Revision, configVersion: config.Version,
	})
	if err != nil {
		return nil, err
	}
	owned, cancel := context.WithCancel(lifetime)
	return &Lease{manager: m, worker: worker, client: client, group: client.group, ctx: owned, cancel: cancel}, nil
}

// BeginGateway runs only explicit, origin-eligible SDK hooks. A failed hook
// keeps the last accepted body and is excluded from completion notification.
func (m *Manager) BeginGateway(ctx context.Context, body []byte, info sdk.GatewayContext) *GatewayRun {
	if ctx == nil || ctx.Err() != nil || len(body) > gatewayBodyLimit || !gatewayObject(body) {
		return nil
	}
	entries := m.gatewayEntries(info.Origin, false)
	if len(entries) == 0 {
		return nil
	}
	run := m.openGatewayRun(ctx, body, info, gatewayRunLifetime)
	if run == nil {
		return nil
	}
	whole, cancel := context.WithTimeout(ctx, max(11*time.Second, gatewayHookBudget(entries)+time.Second))
	defer cancel()
	stop := context.AfterFunc(run.ctx, cancel)
	defer stop()
	for _, entry := range entries {
		if whole.Err() != nil {
			break
		}
		lease, err := m.acquireGatewayLease(whole, run.ctx, entry)
		if err != nil {
			continue
		}
		hooks := *entry.Manifest.Hooks
		state := json.RawMessage(nil)
		if hooks.Request {
			call, cancel := context.WithTimeout(whole, gatewayHookTimeout(hooks))
			result, err := lease.Call(call, sdk.MethodGatewayRequest, sdk.Scope{Session: info.Session},
				sdk.GatewayRequest{Body: bytes.Clone(run.Body), Context: info}, nil)
			cancel()
			var response sdk.GatewayRequestResult
			if err != nil || !decodeGatewayResult(result, &response) || len(response.State) > sdk.MaxGatewayStateBytes ||
				len(response.State) > 0 && !json.Valid(response.State) {
				lease.Close()
				continue
			}
			if len(response.Body) > 0 {
				if len(response.Body) > gatewayBodyLimit || !gatewayHookChange(run.Body, response.Body) {
					lease.Close()
					continue
				}
				run.Body = bytes.Clone(response.Body)
			}
			state = bytes.Clone(response.State)
		}
		run.mu.Lock()
		if run.closed {
			run.mu.Unlock()
			lease.Close()
			break
		}
		run.calls = append(run.calls, gatewayHookCall{lease: lease, descriptor: hooks, state: state})
		run.mu.Unlock()
	}
	run.mu.Lock()
	empty := len(run.calls) == 0 || run.closed
	run.mu.Unlock()
	if empty || ctx.Err() != nil {
		run.abort()
		return nil
	}
	return run
}

func gatewayObject(body []byte) bool {
	body = bytes.TrimSpace(body)
	return len(body) != 0 && body[0] == '{' && json.Valid(body)
}

func decodeGatewayResult(raw json.RawMessage, result *sdk.GatewayRequestResult) bool {
	if len(raw) > sdk.MaxFrameBytes {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(result) == nil && decoder.Decode(new(any)) == io.EOF
}

// Context enrichment cannot alter the model already authorized by the
// gateway, the client's tools/choice, or its requested response framing.
func gatewayHookChange(before, after []byte) bool {
	var old, next map[string]any
	decode := func(body []byte, target *map[string]any) bool {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		return decoder.Decode(target) == nil && decoder.Decode(new(any)) == io.EOF && *target != nil
	}
	if !decode(before, &old) || !decode(after, &next) {
		return false
	}
	for _, key := range []string{"model", "stream", "tools", "tool_choice", "function_call", "functions", "toolConfig"} {
		a, haveA := old[key]
		b, haveB := next[key]
		if haveA != haveB || !reflect.DeepEqual(a, b) {
			return false
		}
	}
	oldConfig, _ := old["config"].(map[string]any)
	newConfig, _ := next["config"].(map[string]any)
	for _, key := range []string{"tools", "toolConfig"} {
		a, haveA := oldConfig[key]
		b, haveB := newConfig[key]
		if haveA != haveB || !reflect.DeepEqual(a, b) {
			return false
		}
	}
	return true
}

func (run *GatewayRun) Finish(_ context.Context, reply json.RawMessage, success bool, request ...json.RawMessage) {
	if run == nil {
		return
	}
	run.mu.Lock()
	if run.closed || run.finish {
		run.mu.Unlock()
		return
	}
	run.finish = true
	calls := slices.Clone(run.calls)
	run.mu.Unlock()
	body := bytes.Clone(run.Body)
	if len(request) != 0 && len(request[0]) <= gatewayBodyLimit && gatewayObject(request[0]) {
		body = bytes.Clone(request[0])
	}
	if len(reply) > 8<<20 || !json.Valid(reply) {
		reply, success = json.RawMessage("null"), false
	}
	reply = bytes.Clone(reply)
	go func() {
		defer run.release()
		whole, cancelWhole := context.WithTimeout(run.ctx, gatewayCaptureBudget(calls)+time.Second)
		defer cancelWhole()
		for _, hook := range calls {
			if !hook.descriptor.Response || whole.Err() != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(whole, gatewayHookTimeout(hook.descriptor))
			_, _ = hook.lease.Call(ctx, sdk.MethodGatewayResponse, sdk.Scope{Session: run.info.Session},
				sdk.GatewayResponse{
					Request: body, Reply: reply, Success: success,
					State: hook.state, Context: run.info,
				}, nil)
			cancel()
		}
	}()
}

// Close abandons a run which never reached Finish. The normal deferred Close
// after Finish leaves the owned observer running until its bounded deadline.
func (run *GatewayRun) Close() {
	if run == nil {
		return
	}
	run.mu.Lock()
	finished := run.finish
	run.mu.Unlock()
	if !finished {
		run.abort()
	}
}

func (run *GatewayRun) abort() {
	run.mu.Lock()
	finished := run.finish
	run.closed = true
	run.mu.Unlock()
	run.cancel()
	if !finished {
		run.release()
	}
}

func (run *GatewayRun) release() {
	run.once.Do(func() {
		run.mu.Lock()
		run.closed = true
		calls := slices.Clone(run.calls)
		run.mu.Unlock()
		run.stop()
		run.expiry()
		run.cancel()
		for _, call := range calls {
			call.lease.Close()
		}
		run.manager.mu.Lock()
		delete(run.manager.gatewayRuns, run)
		run.manager.mu.Unlock()
		close(run.done)
	})
}

// GatewaySignIn is an owned, bounded best-effort notification. Opting into
// sign-in events never grants access to account credentials.
func (m *Manager) GatewaySignIn(ctx context.Context, event sdk.GatewaySignInEvent) {
	if ctx == nil || ctx.Err() != nil {
		return
	}
	entries := m.gatewayEntries("local", true)
	if len(entries) == 0 {
		return
	}
	run := m.openGatewayRun(ctx, nil, sdk.GatewayContext{Origin: "local"}, gatewayHookBudget(entries)+11*time.Second)
	if run == nil {
		return
	}
	run.mu.Lock()
	run.finish = true
	run.mu.Unlock()
	go func() {
		defer run.release()
		for _, entry := range entries {
			if run.ctx.Err() != nil {
				return
			}
			lease, err := m.acquireGatewayLease(run.ctx, run.ctx, entry)
			if err != nil {
				continue
			}
			run.mu.Lock()
			if run.closed {
				run.mu.Unlock()
				lease.Close()
				return
			}
			hook := *entry.Manifest.Hooks
			run.calls = append(run.calls, gatewayHookCall{lease: lease, descriptor: hook})
			run.mu.Unlock()
			call, cancel := context.WithTimeout(run.ctx, gatewayHookTimeout(hook))
			_, _ = lease.Call(call, sdk.MethodGatewaySignIn, sdk.Scope{}, sdk.GatewaySignIn{Event: event}, nil)
			cancel()
		}
	}()
}

func validateGatewayScope(method string, scope sdk.Scope) error {
	if sdk.IsGatewayMethod(method) && (scope.Provider != "" || scope.Account != "" || scope.Generation != 0) {
		return errors.New("gateway hooks do not receive provider or account credentials")
	}
	return nil
}
