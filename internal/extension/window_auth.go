package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type windowAuthParentKey struct{}
type windowAuthPolicyKey struct{}
type WindowAuthPolicy func(context.Context, sdk.Scope) (context.Context, error)

func WithWindowAuthParent(ctx context.Context, parent string) context.Context {
	return context.WithValue(ctx, windowAuthParentKey{}, parent)
}

func WithWindowAuthPolicy(ctx context.Context, policy WindowAuthPolicy) context.Context {
	return context.WithValue(ctx, windowAuthPolicyKey{}, policy)
}

type windowAuthTask struct {
	start   bool
	caller  context.Context
	options sdk.WindowAuthStartOptions
	done    chan struct{}
	view    sdk.WindowAuthFlowView
	err     error
}

type windowAuthOwner struct {
	manager *Manager
	lease   *Lease
	record  windowAuthRecord
	ctx     context.Context
	cancel  context.CancelFunc
	policy  WindowAuthPolicy
	tasks   chan *windowAuthTask
	done    chan struct{}
}

func windowAuthIdentity(ctx context.Context, intent bool) (string, string, error) {
	parent, _ := ctx.Value(windowAuthParentKey{}).(string)
	request, _ := ctx.Value(moduleRequestKey{}).(string)
	if !storeToken(parent) || intent && !storeToken(request) {
		return "", "", errors.New("window authentication requires its parent request identity")
	}
	return parent, request, nil
}

func (m *Manager) windowAuthGrant(ctx context.Context, id, revision, part, binding string) (Installed, sdk.ConfigSnapshot, sdk.WindowAuthBindingDescriptor, error) {
	entry, config, err := m.authEntry(ctx, id, revision, part)
	if err != nil {
		return entry, config, sdk.WindowAuthBindingDescriptor{}, errors.New("authentication view changed or is unavailable")
	}
	grant, found := entry.Manifest.ResolveAuthBinding(binding)
	if !found || grant.From != part {
		return entry, config, grant, errors.New("authentication binding is not granted")
	}
	return entry, config, grant, nil
}

func (m *Manager) CoreAuthStart(ctx context.Context, id, revision, part, binding string, options sdk.WindowAuthStartOptions) (sdk.WindowAuthFlowView, error) {
	parent, request, err := windowAuthIdentity(ctx, true)
	if err != nil {
		return sdk.WindowAuthFlowView{}, err
	}
	entry, config, grant, err := m.windowAuthGrant(ctx, id, revision, part, binding)
	if err != nil {
		return sdk.WindowAuthFlowView{}, err
	}
	method := slices.IndexFunc(grant.Methods, func(method sdk.WindowAuthMethod) bool {
		return method.ID == options.Method && method.Mode == "poll"
	})
	if options.Validate() != nil || method < 0 {
		return sdk.WindowAuthFlowView{}, errors.New("window authentication method is not granted")
	}
	if part == sdk.ProviderSignInPart && !providerSignInInput(grant.Methods[method], options.Input) {
		return sdk.WindowAuthFlowView{}, errors.New("provider sign-in input does not match its declared fields")
	}
	scope := sdk.Scope{Plugin: id, Provider: grant.Provider}
	if options.Account != nil {
		generation, err := options.Account.Validate()
		if !grant.Reauthorize || err != nil {
			return sdk.WindowAuthFlowView{}, errors.New("window authentication does not grant selected reauthorization")
		}
		scope.Account, scope.Generation = options.Account.ID, generation
	}
	if _, err := m.authForContext(ctx, scope); err != nil {
		if ctx.Err() != nil {
			return sdk.WindowAuthFlowView{}, ctx.Err()
		}
		return sdk.WindowAuthFlowView{}, errors.New("window authentication selected account changed")
	}
	policy, supported := ctx.Value(windowAuthPolicyKey{}).(WindowAuthPolicy)
	if !supported || policy == nil {
		return sdk.WindowAuthFlowView{}, errors.New("window authentication requires its host policy owner")
	}
	if _, err := policy(ctx, scope); err != nil {
		return sdk.WindowAuthFlowView{}, errors.New("window authentication policy refuses this operation")
	}
	expires := time.Now().Add(10 * time.Minute)
	owned, cancel := context.WithDeadline(context.WithoutCancel(ctx), expires)
	stopCaller := context.AfterFunc(ctx, cancel)
	lease, err := m.acquireLease(owned, id, &expectedGeneration{
		packageRevision: entry.Revision, configVersion: config.Version, enableGeneration: &entry.EnableGeneration,
	})
	stopCaller()
	if err != nil {
		cancel()
		return sdk.WindowAuthFlowView{}, errors.New("window authentication backend is unavailable")
	}
	defer func() {
		if lease != nil {
			cancel()
			lease.Close()
		}
	}()
	if ctx.Err() != nil {
		return sdk.WindowAuthFlowView{}, ctx.Err()
	}
	input := bytes.Clone(options.Input)
	if len(input) == 0 {
		input = json.RawMessage("null")
	}
	var compact bytes.Buffer
	if json.Compact(&compact, input) != nil {
		return sdk.WindowAuthFlowView{}, errors.New("window authentication input is invalid")
	}
	selected, _ := json.Marshal(scope)
	nonce, err := randomID()
	if err != nil {
		return sdk.WindowAuthFlowView{}, errors.New("window authentication identity is unavailable")
	}
	journalPart, journalRevision := windowAuthJournalSelection(entry, config.Version, part)
	record := windowAuthRecord{
		Flow: storeHash(nonce), Intent: storeHash(id, revision, part, parent, request),
		Fingerprint: storeHash(binding, options.Method, string(selected), compact.String()),
		Parent:      parent, Revision: journalRevision, Config: config.Version,
		Part: journalPart, Binding: binding, Method: options.Method, Scope: scope,
		Phase: "reserved", Created: expires.Add(-10 * time.Minute).UnixMilli(), Expires: expires.UnixMilli(),
	}
	options.Input = input
	var created bool
	err = m.withWindowAuthAuthority(ctx, record, func() error {
		var err error
		record, created, err = m.store.reserveWindowAuth(record)
		return err
	})
	if err != nil {
		return sdk.WindowAuthFlowView{}, errors.New("window authentication intent could not be admitted")
	}
	m.mu.Lock()
	owner := m.windowAuthOwners[record.Flow]
	if !created {
		m.mu.Unlock()
		if owner != nil {
			return owner.submit(ctx, true, options)
		}
		if record.Phase == "complete" || record.Phase == "canceled" || record.Phase == "expired" || record.Phase == "uncertain" {
			return record.public(), nil
		}
		return sdk.WindowAuthFlowView{Flow: record.Flow, State: "uncertain"}, nil
	}
	if m.closed || len(m.windowAuthOwners) >= 64 {
		m.mu.Unlock()
		return sdk.WindowAuthFlowView{}, errors.New("window authentication owner capacity is unavailable")
	}
	stop := context.AfterFunc(m.startCtx, cancel)
	owner = &windowAuthOwner{
		manager: m, lease: lease, record: record, ctx: owned, cancel: cancel, policy: policy,
		tasks: make(chan *windowAuthTask), done: make(chan struct{}),
	}
	if m.windowAuthOwners == nil {
		m.windowAuthOwners = map[string]*windowAuthOwner{}
	}
	m.windowAuthOwners[record.Flow] = owner
	m.windowAuthWG.Add(1)
	lease = nil
	m.mu.Unlock()
	go owner.run(stop)
	return owner.submit(ctx, true, options)
}
func (m *Manager) windowAuthFlow(ctx context.Context, id, revision, part, binding, flow string) (windowAuthRecord, *windowAuthOwner, error) {
	parent, _, err := windowAuthIdentity(ctx, false)
	if err != nil {
		return windowAuthRecord{}, nil, err
	}
	entry, config, grant, err := m.windowAuthGrant(ctx, id, revision, part, binding)
	if err != nil {
		return windowAuthRecord{}, nil, err
	}
	policy, supported := ctx.Value(windowAuthPolicyKey{}).(WindowAuthPolicy)
	if !supported || policy == nil {
		return windowAuthRecord{}, nil, errors.New("window authentication requires its host policy owner")
	}
	if _, err := policy(ctx, sdk.Scope{Plugin: id, Provider: grant.Provider}); err != nil {
		return windowAuthRecord{}, nil, errors.New("window authentication policy refuses this operation")
	}
	record, err := m.store.windowAuthRecord(id, flow)
	journalPart, journalRevision := windowAuthJournalSelection(entry, config.Version, part)
	if err != nil || record.Parent != parent || record.Part != journalPart || record.Binding != binding ||
		record.Revision != journalRevision || record.Config != config.Version || record.Scope.Provider != grant.Provider {
		return windowAuthRecord{}, nil, errors.New("window authentication flow does not belong to this view")
	}
	if _, err := policy(ctx, record.Scope); err != nil {
		return windowAuthRecord{}, nil, errors.New("window authentication policy refuses this operation")
	}
	m.mu.Lock()
	owner := m.windowAuthOwners[flow]
	m.mu.Unlock()
	return record, owner, nil
}

func (m *Manager) CoreAuthFinish(ctx context.Context, id, revision, part, binding, flow string) (sdk.WindowAuthFlowView, error) {
	record, owner, err := m.windowAuthFlow(ctx, id, revision, part, binding, flow)
	if err != nil {
		return sdk.WindowAuthFlowView{}, err
	}
	if owner == nil {
		switch record.Phase {
		case "complete", "canceled", "expired", "uncertain":
			return record.public(), nil
		default:
			return sdk.WindowAuthFlowView{Flow: flow, State: "uncertain"}, nil
		}
	}
	return owner.submit(ctx, false, sdk.WindowAuthStartOptions{})
}

func (m *Manager) CoreAuthCancel(ctx context.Context, id, revision, part, binding, flow string) (sdk.WindowAuthFlowView, error) {
	record, owner, err := m.windowAuthFlow(ctx, id, revision, part, binding, flow)
	if err != nil {
		return sdk.WindowAuthFlowView{}, err
	}
	if record.Phase == "complete" || record.Phase == "uncertain" || record.Phase == "canceled" || record.Phase == "expired" {
		return record.public(), nil
	}
	record, err = m.store.updateWindowAuth(id, flow, func(record *windowAuthRecord, _ *storeAccounts) error {
		if record.Phase == "complete" || record.Phase == "uncertain" {
			return nil
		}
		record.Canceled = true
		if record.Phase == "waiting" || record.Phase == "reserved" {
			record.Phase, record.Continuation = "canceled", nil
		}
		return nil
	})
	if err != nil {
		return sdk.WindowAuthFlowView{}, errors.New("window authentication cancellation could not be recorded")
	}
	if owner != nil && record.Phase == "canceled" {
		owner.cancel()
	}
	return record.public(), nil
}

func (owner *windowAuthOwner) submit(ctx context.Context, start bool, options sdk.WindowAuthStartOptions) (sdk.WindowAuthFlowView, error) {
	task := &windowAuthTask{start: start, caller: ctx, options: options, done: make(chan struct{})}
	select {
	case owner.tasks <- task:
	case <-ctx.Done():
		return sdk.WindowAuthFlowView{}, ctx.Err()
	case <-owner.done:
		record, err := owner.manager.store.windowAuthRecord(owner.record.Scope.Plugin, owner.record.Flow)
		if err != nil {
			return sdk.WindowAuthFlowView{}, errors.New("window authentication receipt is unavailable")
		}
		return record.public(), nil
	}
	select {
	case <-task.done:
		return task.view, task.err
	case <-ctx.Done():
		return sdk.WindowAuthFlowView{}, ctx.Err()
	}
}

func (owner *windowAuthOwner) run(stop func() bool) {
	defer func() {
		stop()
		owner.cancel()
		owner.lease.Close()
		owner.manager.mu.Lock()
		delete(owner.manager.windowAuthOwners, owner.record.Flow)
		owner.manager.mu.Unlock()
		close(owner.done)
		owner.manager.windowAuthWG.Done()
	}()
	for {
		select {
		case <-owner.ctx.Done():
			_, _ = owner.manager.store.updateWindowAuth(owner.record.Scope.Plugin, owner.record.Flow, func(record *windowAuthRecord, _ *storeAccounts) error {
				if record.Phase == "reserved" || record.Phase == "waiting" {
					record.Phase, record.Continuation = "expired", nil
				}
				return nil
			})
			return
		case task := <-owner.tasks:
			task.view, task.err = owner.execute(task)
			close(task.done)
			if task.view.State == "complete" || task.view.State == "uncertain" || task.view.State == "canceled" || task.view.State == "expired" {
				return
			}
		}
	}
}

func (owner *windowAuthOwner) execute(task *windowAuthTask) (sdk.WindowAuthFlowView, error) {
	manager, plugin, flow := owner.manager, owner.record.Scope.Plugin, owner.record.Flow
	record, err := manager.store.windowAuthRecord(plugin, flow)
	if err != nil {
		return sdk.WindowAuthFlowView{}, errors.New("window authentication state is unavailable")
	}
	if task.caller.Err() != nil {
		return sdk.WindowAuthFlowView{}, task.caller.Err()
	}
	if record.Canceled || record.Phase == "complete" || record.Phase == "canceled" || record.Phase == "expired" || record.Phase == "uncertain" {
		return record.public(), nil
	}
	if record.Phase == "waiting" && (task.start || time.Now().UnixMilli() < record.Next) {
		return record.public(), nil
	}
	if task.start && record.Phase != "reserved" || !task.start && record.Phase != "waiting" {
		return sdk.WindowAuthFlowView{Flow: flow, State: "uncertain"}, nil
	}
	ctx, cancel := context.WithTimeout(owner.ctx, 60*time.Second)
	defer cancel()
	ctx, err = owner.policy(ctx, record.Scope)
	if err != nil {
		return sdk.WindowAuthFlowView{}, errors.New("window authentication policy refuses this operation")
	}
	if !task.start && record.Scope.Account != "" {
		unlock, err := manager.store.lockRefresh(ctx, record.Scope)
		if err != nil {
			return sdk.WindowAuthFlowView{}, errors.New("window authentication selected account is occupied")
		}
		defer unlock()
		if _, err := manager.store.refreshStatus(record.Scope); err != nil {
			var failure *sdk.Error
			if !errors.As(err, &failure) || failure.Code != "refresh_uncertain" {
				return sdk.WindowAuthFlowView{}, errors.New("window authentication selected account changed")
			}
		}
	}
	ctx = context.WithValue(ctx, windowAdmissionKey{}, func(current context.Context) error {
		return manager.withWindowAuthAuthority(current, record, func() error { return nil })
	})
	err = manager.withWindowAuthAuthority(ctx, record, func() error {
		if err := task.caller.Err(); err != nil {
			return err
		}
		var err error
		record, err = manager.store.updateWindowAuth(plugin, flow, func(current *windowAuthRecord, _ *storeAccounts) error {
			if current.Canceled || current.Phase != record.Phase || time.Now().UnixMilli() >= current.Expires {
				return errors.New("window authentication dispatch authority changed")
			}
			if task.start {
				current.Phase = "starting"
			} else {
				current.Phase = "polling"
			}
			return nil
		})
		return err
	})
	if err != nil {
		return sdk.WindowAuthFlowView{}, errors.New("window authentication authority changed before dispatch")
	}
	owner.lease.worker.mu.Lock()
	owner.lease.worker.refreshing++
	owner.lease.worker.mu.Unlock()
	defer func() {
		owner.lease.worker.mu.Lock()
		owner.lease.worker.refreshing--
		owner.lease.worker.mu.Unlock()
	}()
	method, params := sdk.MethodAuthFinish, any(sdk.AuthFinishRequest{Continuation: bytes.Clone(record.Continuation)})
	if task.start {
		method, params = sdk.MethodAuthStart, sdk.AuthStartRequest{Method: record.Method, Input: bytes.Clone(task.options.Input)}
	}
	raw, callErr := owner.lease.Call(ctx, method, record.Scope, params, func(Event) error {
		return errors.New("window authentication requires a unary result")
	})
	if callErr == nil && task.start {
		var result sdk.AuthStartResult
		if json.Unmarshal(raw, &result) == nil && len(result.Continuation) != 0 &&
			len(result.Continuation) <= sdk.MaxHostBytes && json.Valid(result.Continuation) && string(result.Continuation) != "null" {
			interval := max(int64(1000), min(result.PollAfterMs, int64(300_000)))
			view := sdk.WindowAuthFlowView{Flow: flow, State: "waiting", URL: result.URL, UserCode: result.UserCode, RetryAfterMs: interval}
			if view.Validate() == nil {
				err = manager.withWindowAuthAuthority(ctx, record, func() error {
					var err error
					record, err = manager.store.updateWindowAuth(plugin, flow, func(current *windowAuthRecord, _ *storeAccounts) error {
						if current.Canceled || current.Phase != "starting" {
							return errors.New("window authentication start was revoked")
						}
						current.Phase, current.URL, current.UserCode = "waiting", result.URL, result.UserCode
						current.Continuation, current.Interval, current.Next = bytes.Clone(result.Continuation), interval, time.Now().UnixMilli()+interval
						return nil
					})
					return err
				})
				if err == nil {
					return record.public(), nil
				}
			}
		}
	} else if callErr == nil {
		var result sdk.AuthResult
		if json.Unmarshal(raw, &result) == nil {
			if !result.Pending && result.ID == "" {
				result.ID = record.Scope.Account
			}
			if result.Pending && len(result.Credentials) == 0 && result.ID == "" {
				err = manager.withWindowAuthAuthority(ctx, record, func() error {
					var err error
					record, err = manager.store.updateWindowAuth(plugin, flow, func(current *windowAuthRecord, _ *storeAccounts) error {
						if current.Canceled || current.Phase != "polling" {
							return errors.New("window authentication poll was revoked")
						}
						current.Phase, current.Next = "waiting", time.Now().UnixMilli()+current.Interval
						return nil
					})
					return err
				})
				if err == nil {
					return record.public(), nil
				}
			} else if !result.Pending {
				err = manager.withWindowAuthAuthority(ctx, record, func() error {
					var err error
					record, err = manager.store.publishWindowAuth(plugin, flow, result)
					return err
				})
				if err == nil {
					notifyChanged()
					return record.public(), nil
				}
				if len(result.Credentials) <= sdk.MaxHostBytes && json.Valid(result.Credentials) {
					_, _ = manager.store.updateWindowAuth(plugin, flow, func(current *windowAuthRecord, _ *storeAccounts) error {
						current.Phase, current.Private, current.Receipt = "uncertain", &result, nil
						return nil
					})
				}
			}
		}
	}
	_, _ = manager.store.updateWindowAuth(plugin, flow, func(current *windowAuthRecord, _ *storeAccounts) error {
		current.Phase = "uncertain"
		return nil
	})
	return sdk.WindowAuthFlowView{Flow: flow, State: "uncertain"}, nil
}

func (m *Manager) revokeWindowAuthOwners(current map[string]Installed, configs map[string]string) {
	m.mu.Lock()
	owners := make([]*windowAuthOwner, 0, len(m.windowAuthOwners))
	for _, owner := range m.windowAuthOwners {
		owners = append(owners, owner)
	}
	m.mu.Unlock()
	for _, owner := range owners {
		entry, found := current[owner.record.Scope.Plugin]
		config := configs[entry.Manifest.ID]
		if found && config == owner.record.Config {
			if grant, ok := entry.Manifest.ResolveAuthBinding(owner.record.Binding); ok && grant.Provider == owner.record.Scope.Provider {
				part, revision := windowAuthJournalSelection(entry, config, grant.From)
				if part == owner.record.Part && revision == owner.record.Revision {
					continue
				}
			}
		}
		record, err := m.store.updateWindowAuth(owner.record.Scope.Plugin, owner.record.Flow, func(record *windowAuthRecord, _ *storeAccounts) error {
			if record.Phase == "complete" || record.Phase == "uncertain" {
				return nil
			}
			record.Canceled = true
			if record.Phase == "reserved" || record.Phase == "waiting" {
				record.Phase, record.Continuation = "canceled", nil
			}
			return nil
		})
		if err != nil || record.Phase == "canceled" || record.Phase == "complete" || record.Phase == "uncertain" || record.Phase == "expired" {
			owner.cancel()
		}
	}
}

func (m *Manager) withWindowAuthAuthority(ctx context.Context, record windowAuthRecord, action func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if policy, found := ctx.Value(windowAuthPolicyKey{}).(WindowAuthPolicy); found {
		if _, err := policy(ctx, record.Scope); err != nil {
			return err
		}
	}
	root, err := m.catalog.open(false)
	if err != nil {
		return err
	}
	defer root.Close()
	unlock, err := catalogLock(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
	entries, _, err := catalogRead(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Manifest.ID != record.Scope.Plugin || entry.Disabled {
			continue
		}
		grant, found := entry.Manifest.ResolveAuthBinding(record.Binding)
		if !found || grant.Provider != record.Scope.Provider {
			return errors.New("authentication grant changed")
		}
		if grant.From == sdk.ProviderSignInPart {
			entry.EnableGeneration, err = readObserverEnablement(root, record.Scope.Plugin)
			if err != nil {
				return errors.New("authentication enablement changed")
			}
		}
		config, err := m.readModuleConfigContext(ctx, record.Scope.Plugin)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		part, revision := windowAuthJournalSelection(entry, config.Version, grant.From)
		if err != nil || part != record.Part || revision != record.Revision || config.Version != record.Config {
			return errors.New("window authentication selection changed")
		}
		return action()
	}
	return errors.New("window authentication package changed or is disabled")
}
