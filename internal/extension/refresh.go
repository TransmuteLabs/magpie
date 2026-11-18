package extension

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	maxRefreshOwners = 64
	refreshRPCLimit  = 60 * time.Second
	refreshLifetime  = 70 * time.Second
)

type refreshKey struct {
	plugin     string
	provider   string
	account    string
	generation uint64
}

type refreshRun struct {
	mu         sync.Mutex
	dispatched bool
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	receipt    sdk.AuthResult
	err        error
	lease      *Lease
}

func (m *Manager) RefreshAccount(ctx context.Context, id string, scope sdk.Scope) (sdk.AuthResult, error) {
	if scope.Plugin == "" {
		scope.Plugin = id
	}
	if err := ctx.Err(); err != nil {
		return sdk.AuthResult{}, err
	}
	if err := netproxy.Held(ctx); err != nil {
		return sdk.AuthResult{}, err
	}
	lease, err := m.Acquire(ctx, id)
	if err != nil {
		return sdk.AuthResult{}, err
	}
	defer lease.Close()
	return m.refreshFromLease(ctx, lease, scope)
}

func (m *Manager) refreshFromLease(ctx context.Context, lease *Lease, scope sdk.Scope) (sdk.AuthResult, error) {
	if err := validateMethod(lease.worker.entry.Manifest, sdk.MethodAuthRefresh, scope); err != nil {
		return sdk.AuthResult{}, err
	}
	if !storeValidScope(scope) || scope.Account == "" {
		return sdk.AuthResult{}, storeInvalid()
	}
	if err := ctx.Err(); err != nil {
		return sdk.AuthResult{}, err
	}
	if err := netproxy.Held(ctx); err != nil {
		return sdk.AuthResult{}, err
	}
	key := refreshKey{scope.Plugin, scope.Provider, scope.Account, scope.Generation}
	m.mu.Lock()
	if m.closed || lease.closed.Load() {
		m.mu.Unlock()
		return sdk.AuthResult{}, errors.New("extension refresh owner is closed")
	}
	run := m.refreshes[key]
	if run == nil {
		if len(m.refreshes) >= maxRefreshOwners {
			m.mu.Unlock()
			return sdk.AuthResult{}, storeError("busy", "Extension credential refresh capacity is occupied.")
		}
		m.startLifecycleLocked()
		owned, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshLifetime)
		worker := lease.worker
		worker.mu.Lock()
		if lease.closed.Load() {
			worker.mu.Unlock()
			cancel()
			m.mu.Unlock()
			return sdk.AuthResult{}, errors.New("extension refresh lease is closed")
		}
		if worker.active == 0 {
			worker.idle = make(chan struct{})
		}
		worker.active++
		worker.refreshing++
		worker.mu.Unlock()
		pinned, stop := context.WithCancel(owned)
		run = &refreshRun{ctx: owned, cancel: cancel, done: make(chan struct{}), lease: &Lease{
			manager: m, worker: worker, client: lease.client, group: lease.group, ctx: pinned, cancel: stop,
		}}
		if m.refreshes == nil {
			m.refreshes = make(map[refreshKey]*refreshRun)
		}
		m.refreshes[key] = run
		m.refreshWG.Add(1)
		go m.ownRefresh(ctx, key, scope, run)
	}
	m.mu.Unlock()
	select {
	case <-run.done:
		return run.receipt, run.err
	case <-ctx.Done():
		return sdk.AuthResult{}, ctx.Err()
	}
}

func (m *Manager) ownRefresh(caller context.Context, key refreshKey, scope sdk.Scope, run *refreshRun) {
	stopCaller := context.AfterFunc(caller, func() {
		run.mu.Lock()
		defer run.mu.Unlock()
		if !run.dispatched {
			run.cancel()
		}
	})
	stopManager := context.AfterFunc(m.startCtx, run.cancel)
	defer func() {
		stopCaller()
		stopManager()
		run.cancel()
		run.lease.worker.mu.Lock()
		run.lease.worker.refreshing--
		run.lease.worker.mu.Unlock()
		run.lease.Close()
		m.mu.Lock()
		delete(m.refreshes, key)
		close(run.done)
		m.mu.Unlock()
		m.refreshWG.Done()
	}()
	run.receipt, run.err = m.executeRefresh(caller, scope, run)
	if run.err == nil {
		notifyChanged()
	}
}

func (m *Manager) executeRefresh(caller context.Context, scope sdk.Scope, run *refreshRun) (sdk.AuthResult, error) {
	unlock, err := m.store.lockRefresh(run.ctx, scope)
	if err != nil {
		return sdk.AuthResult{}, err
	}
	defer unlock()
	if receipt, err := m.store.refreshStatus(scope); err != nil || receipt != nil {
		if receipt != nil {
			return *receipt, err
		}
		return sdk.AuthResult{}, err
	}
	operation, err := randomID()
	if err != nil {
		return sdk.AuthResult{}, err
	}
	scope.Session, scope.Operation, scope.Attempt = "", "refresh-"+operation, 1
	run.mu.Lock()
	if err := caller.Err(); err != nil {
		run.mu.Unlock()
		return sdk.AuthResult{}, err
	}
	if err := run.ctx.Err(); err != nil {
		run.mu.Unlock()
		return sdk.AuthResult{}, err
	}
	account, err := m.store.claimRefresh(scope, run.lease.Revision(), run.lease.ConfigVersion())
	if err != nil {
		run.mu.Unlock()
		return sdk.AuthResult{}, err
	}
	if err := caller.Err(); err != nil {
		run.mu.Unlock()
		_, _ = m.store.finishRefresh(scope, nil, "not-started")
		return sdk.AuthResult{}, err
	}
	run.dispatched = true
	run.mu.Unlock()
	ctx, cancel := context.WithTimeout(run.ctx, refreshRPCLimit)
	defer cancel()
	ctx = context.WithValue(ctx, hostAuthorityKey{}, hostAuthority{
		manifest: run.lease.worker.entry.Manifest, method: sdk.MethodAuthRefresh,
	})
	if group := run.lease.group; group != nil {
		ctx = group.withAuthority(ctx, sdk.PrimaryComponent, "write")
	}
	raw, callErr := run.lease.client.call(ctx, sdk.MethodAuthRefresh, scope, account.Auth, sdk.AuthRefreshRequest{},
		func(Event) error { return errors.New("credential refresh must return one unary result") })
	if callErr == nil {
		if result, err := decodeRefreshResult(scope, raw); err == nil {
			receipt, err := m.store.finishRefresh(scope, &result, "completed")
			if err == nil {
				return receipt, nil
			}
		}
	}
	state := "uncertain"
	var outcome *OutcomeError
	var rpc *sdk.Error
	if errors.As(callErr, &outcome) && outcome.Outcome == "not-started" ||
		errors.As(callErr, &rpc) && rpc.Outcome == "not-started" {
		state = "not-started"
	}
	_, _ = m.store.finishRefresh(scope, nil, state)
	cause := errors.New("extension credential refresh did not publish replacement credentials")
	if errors.Is(callErr, context.Canceled) {
		cause = context.Canceled
	} else if errors.Is(callErr, context.DeadlineExceeded) {
		cause = context.DeadlineExceeded
	}
	return sdk.AuthResult{}, &OutcomeError{Outcome: state, Cause: cause}
}
