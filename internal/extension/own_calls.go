package extension

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type ownCallParentKey struct{}

type ownCallParent struct {
	mu       sync.Mutex
	method   string
	scope    sdk.Scope
	login    PublicSubscriptionLogin
	appended bool
	reported bool
	snapshot *sdk.OwnCall
}

var ownCalls struct {
	sync.RWMutex
	handler func(context.Context, sdk.Scope, sdk.OwnCall)
}

func OnOwnCall(handler func(sdk.Scope, sdk.OwnCall)) func() {
	return OnOwnCallContext(func(_ context.Context, scope sdk.Scope, call sdk.OwnCall) { handler(scope, call) })
}

func OnOwnCallContext(handler func(context.Context, sdk.Scope, sdk.OwnCall)) func() {
	ownCalls.Lock()
	previous := ownCalls.handler
	ownCalls.handler = handler
	ownCalls.Unlock()
	return func() {
		ownCalls.Lock()
		ownCalls.handler = previous
		ownCalls.Unlock()
	}
}

func withOwnCallParent(ctx context.Context, method string) context.Context {
	if ctx.Value(ownCallParentKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, ownCallParentKey{}, &ownCallParent{method: method})
}

func OwnCallPublicLogin(ctx context.Context) (provider, user string) {
	parent, _ := ctx.Value(ownCallParentKey{}).(*ownCallParent)
	if parent == nil {
		return "", ""
	}
	return parent.login.Provider, parent.login.User
}

func (m *Manager) ownCallAdmission(ctx context.Context, scope sdk.Scope, admit workerAdmission) workerAdmission {
	parent := ctx.Value(ownCallParentKey{}).(*ownCallParent)
	if parent.login.Visible {
		return admit
	}
	return func(ctx context.Context) (func(bool), error) {
		var release func(bool)
		if admit != nil {
			var err error
			release, err = admit(ctx)
			if err != nil {
				return nil, err
			}
		}
		owners, err := m.store.SubscriptionOwners(ctx)
		if err == nil {
			login := ResolveSubscriptionLogin(owners, scope)
			if rev, bound := SubscriptionOwnerRevision(ctx); !login.Visible || bound && login.Revision != rev {
				err = errSubscriptionChanged
			} else {
				parent.scope, parent.login = scope, login
			}
		}
		if err != nil {
			if release != nil {
				release(true)
			}
			return nil, err
		}
		return release, nil
	}
}

func (m *Manager) appendOwnCall(ctx context.Context, scope sdk.Scope, raw json.RawMessage, authority hostAuthority) (any, error) {
	return m.recordOwnCall(ctx, scope, raw, authority, false)
}

func (m *Manager) recordOwnCall(ctx context.Context, scope sdk.Scope, raw json.RawMessage, authority hostAuthority, snapshot bool) (any, error) {
	parent, live := ctx.Value(ownCallParentKey{}).(*ownCallParent)
	component, owned := ctx.Value(componentAuthorityKey{}).(componentAuthority)
	readOnly, _ := ctx.Value(gatewayReadOnlyKey{}).(bool)
	var request sdk.OwnCallRequest
	if !live || !owned || component.group == nil || component.effect != "write" || readOnly ||
		!parent.login.Visible || parent.scope != scope ||
		parent.method != sdk.MethodModuleCall && parent.method != sdk.MethodToolCall ||
		authority.method != sdk.MethodModuleCall && authority.method != sdk.MethodToolCall ||
		!decodeHostParams(raw, &request) || sdk.ValidateOwnCall(authority.manifest, request.Call) != nil ||
		scope.Provider == "" || scope.Operation == "" || scope.Attempt == 0 ||
		authority.manifest.Contract.Has(sdk.AccountAuth) && scope.Account == "" {
		return nil, errors.New("own-call accounting requires a selected accepted write intent")
	}
	if err := validateCallScope(authority.manifest, authority.method, scope); err != nil {
		return nil, err
	}
	group := component.group
	if group.ctx.Err() != nil {
		return nil, ErrActionChanged
	}
	if err := m.checkActiveGeneration(ctx, group.entry, group.config.Version); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrActionChanged
	}
	if err := group.accepted(ctx, scope); err != nil {
		return nil, err
	}
	task, scheduled := ctx.Value(accountTaskExecutionKey{}).(*accountTaskExecution)
	if snapshot && !scheduled {
		return nil, errors.New("usage snapshots require a host-owned paid task")
	}
	if scheduled && (request.Call.Model != task.parent.selection.Model || task.scope != scope) {
		return nil, errors.New("paid task usage must name its actual selected model")
	}
	ownCalls.RLock()
	handler := ownCalls.handler
	ownCalls.RUnlock()
	if handler == nil {
		return nil, errors.New("ordinary usage accounting is unavailable")
	}
	if err := filelock.LockMutex(ctx, &parent.mu); err != nil {
		return nil, err
	}
	if parent.appended || scheduled && parent.reported && !snapshot {
		parent.mu.Unlock()
		return sdk.OwnCallResult{Accepted: false}, nil
	}
	if err := ctx.Err(); err != nil {
		parent.mu.Unlock()
		return nil, err
	}
	if _, err := m.authForContext(ctx, scope); err != nil {
		parent.mu.Unlock()
		return nil, err
	}
	request.Call.Usage = *request.Call.Usage.Clone()
	if scheduled {
		parent.snapshot = &request.Call
		parent.reported = parent.reported || !snapshot
		parent.mu.Unlock()
		return sdk.OwnCallResult{Accepted: true}, nil
	}
	parent.appended = true
	parent.mu.Unlock()
	handler(ctx, scope, request.Call)
	return sdk.OwnCallResult{Accepted: true}, nil
}
