package extension

import (
	"context"
	"encoding/json"
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
	owner      string
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

type toolRefreshCheckKey struct{}

type toolRefreshCheck struct {
	selected ToolAccountSelection
	check    func(context.Context) error
}

func WithToolRefreshCheck(ctx context.Context, selected ToolAccountSelection, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, toolRefreshCheckKey{}, toolRefreshCheck{selected: selected, check: check})
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
	if tool, ok := ctx.Value(toolRefreshCheckKey{}).(toolRefreshCheck); ok {
		if m.Namespace() != tool.selected.namespace || !tool.selected.matches(lease.worker.entry) ||
			!sameModuleConfig(lease.worker.config, tool.selected.config) {
			return sdk.AuthResult{}, &OutcomeError{Outcome: "not-started", Cause: errors.New("extension tool refresh lease has changed")}
		}
	}
	if err := m.store.checkSubscriptionView(ctx, scope); err != nil {
		return sdk.AuthResult{}, err
	}
	revision, _ := SubscriptionOwnerRevision(ctx)
	key := refreshKey{scope.Plugin, scope.Provider, scope.Account, scope.Generation, revision}
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
	operation, err := randomID()
	if err != nil {
		return sdk.AuthResult{}, err
	}
	scope.Session, scope.Operation, scope.Attempt = "", "refresh-"+operation, 1
	var cached *sdk.AuthResult
	err = m.withSubscriptionScope(run.ctx, scope, func() error {
		var err error
		cached, err = m.store.refreshStatusContext(run.ctx, scope)
		return err
	})
	if cached != nil {
		return *cached, err
	}
	if err != nil {
		return sdk.AuthResult{}, err
	}
	ctx, cancel := context.WithTimeout(run.ctx, refreshRPCLimit)
	defer cancel()
	ctx = context.WithValue(ctx, hostAuthorityKey{}, hostAuthority{
		manifest: run.lease.worker.entry.Manifest, method: sdk.MethodAuthRefresh,
	})
	if group := run.lease.group; group != nil {
		ctx = group.withAuthority(ctx, sdk.PrimaryComponent, "write")
	}
	var claimed bool
	var claimedAuth json.RawMessage
	raw, callErr := run.lease.client.callWithAdmission(ctx, sdk.MethodAuthRefresh, scope, nil, sdk.AuthRefreshRequest{},
		func(Event) error { return errors.New("credential refresh must return one unary result") }, nil, &workerCallPreparation{
			auth: func(current context.Context) (json.RawMessage, error) {
				account, published, err := run.lease.claimRefresh(current, caller, scope)
				claimed = published
				claimedAuth = account.Auth
				return account.Auth, err
			},
			admit: func(current context.Context, commit func() error) error {
				return run.lease.admitClaimedRefresh(current, scope, claimedAuth, commit)
			},
			begin: func(current context.Context) error {
				run.mu.Lock()
				defer run.mu.Unlock()
				if err := caller.Err(); err != nil {
					return err
				}
				if err := current.Err(); err != nil {
					return err
				}
				run.dispatched = true
				return nil
			},
		})
	if !claimed {
		return sdk.AuthResult{}, callErr
	}
	cause := errors.New("extension credential refresh did not publish replacement credentials")
	if callErr == nil {
		if result, err := decodeRefreshResult(scope, raw); err == nil {
			var receipt sdk.AuthResult
			err := m.withSubscriptionScope(run.ctx, scope, func() error {
				var err error
				receipt, err = m.store.finishRefresh(scope, &result, "completed")
				return err
			})
			if err == nil {
				return receipt, nil
			}
			cause = err
		}
	}
	state := "uncertain"
	var outcome *OutcomeError
	var rpc *sdk.Error
	if errors.As(callErr, &outcome) && outcome.Outcome == "not-started" ||
		errors.As(callErr, &rpc) && rpc.Outcome == "not-started" {
		state = "not-started"
		cause = callErr
	}
	if errors.Is(callErr, context.Canceled) {
		cause = context.Canceled
	} else if errors.Is(callErr, context.DeadlineExceeded) {
		cause = context.DeadlineExceeded
	}
	if _, err := m.store.finishRefresh(scope, nil, state); err != nil {
		return sdk.AuthResult{}, &OutcomeError{Outcome: "uncertain", Cause: errors.Join(err, cause)}
	}
	return sdk.AuthResult{}, &OutcomeError{Outcome: state, Cause: cause}
}

func (lease *Lease) claimRefresh(ctx, caller context.Context, scope sdk.Scope) (Account, bool, error) {
	m := lease.manager
	tool, toolBound := ctx.Value(toolRefreshCheckKey{}).(toolRefreshCheck)
	check := func() error {
		if err := caller.Err(); err != nil {
			return err
		}
		if toolBound {
			return tool.check(ctx)
		}
		return nil
	}
	var account Account
	var published bool
	claim := func() error {
		var err error
		account, published, err = m.store.claimRefreshContext(ctx, scope, lease.Revision(), lease.ConfigVersion(), check)
		return err
	}
	if !toolBound {
		err := m.withSubscriptionScope(ctx, scope, claim)
		return account, published, err
	}
	err := m.withActiveConfig(ctx, lease.worker.entry, tool.selected.config, func() error {
		if err := m.store.checkSubscriptionView(ctx, scope); err != nil {
			return err
		}
		return claim()
	})
	return account, published, err
}

func (lease *Lease) admitClaimedRefresh(ctx context.Context, scope sdk.Scope, auth json.RawMessage, commit func() error) error {
	m := lease.manager
	tool, toolBound := ctx.Value(toolRefreshCheckKey{}).(toolRefreshCheck)
	admit := func() error {
		return m.store.withClaimedRefresh(ctx, scope, lease.Revision(), lease.ConfigVersion(), auth, func() error {
			if toolBound {
				if err := tool.check(ctx); err != nil {
					return err
				}
			}
			return commit()
		})
	}
	if !toolBound {
		return m.withSubscriptionScope(ctx, scope, admit)
	}
	return m.withActiveConfig(ctx, lease.worker.entry, tool.selected.config, func() error {
		if err := m.store.checkSubscriptionView(ctx, scope); err != nil {
			return err
		}
		return admit()
	})
}
