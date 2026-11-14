package extension

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type ownCallParentKey struct{}

type ownCallParent struct {
	mu       sync.Mutex
	method   string
	appended bool
}

var ownCalls struct {
	sync.RWMutex
	handler func(sdk.Scope, sdk.OwnCall)
}

func OnOwnCall(handler func(sdk.Scope, sdk.OwnCall)) func() {
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

func (m *Manager) appendOwnCall(ctx context.Context, scope sdk.Scope, raw json.RawMessage, authority hostAuthority) (any, error) {
	parent, live := ctx.Value(ownCallParentKey{}).(*ownCallParent)
	component, owned := ctx.Value(componentAuthorityKey{}).(componentAuthority)
	readOnly, _ := ctx.Value(gatewayReadOnlyKey{}).(bool)
	var request sdk.OwnCallRequest
	if !live || !owned || component.group == nil || component.effect != "write" || readOnly ||
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
	config, err := m.readModuleConfig(scope.Plugin)
	if err != nil || config.Version != group.config.Version || group.ctx.Err() != nil {
		return nil, ErrActionChanged
	}
	entries, err := m.List()
	if err != nil || !slices.ContainsFunc(entries, func(entry Installed) bool {
		return entry.Manifest.ID == scope.Plugin && !entry.Disabled && entry.Revision == group.entry.Revision
	}) {
		return nil, ErrActionChanged
	}
	if err := group.accepted(scope); err != nil {
		return nil, err
	}
	ownCalls.RLock()
	handler := ownCalls.handler
	ownCalls.RUnlock()
	if handler == nil {
		return nil, errors.New("ordinary usage accounting is unavailable")
	}
	parent.mu.Lock()
	if parent.appended {
		parent.mu.Unlock()
		return sdk.OwnCallResult{Accepted: false}, nil
	}
	if err := ctx.Err(); err != nil {
		parent.mu.Unlock()
		return nil, err
	}
	if _, err := m.authFor(scope); err != nil {
		parent.mu.Unlock()
		return nil, err
	}
	parent.appended = true
	parent.mu.Unlock()
	request.Call.Usage = *request.Call.Usage.Clone()
	handler(scope, request.Call)
	return sdk.OwnCallResult{Accepted: true}, nil
}
