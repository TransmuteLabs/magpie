package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type windowAuthParentKey struct{}
type windowAuthPolicyKey struct{}
type windowAuthAccountPolicyKey struct{}
type windowAuthDispatchKey struct{}
type WindowAuthPolicy func(context.Context, sdk.Scope) (context.Context, error)
type WindowAuthAccountPolicy func(context.Context, *Manager, sdk.Scope, sdk.AccountSummary)

var errWindowAuthUnconfirmed = errors.New("window authentication completion could not be confirmed")

type windowAuthDispatch struct {
	owner  *windowAuthOwner
	record windowAuthRecord
	config sdk.ConfigSnapshot
}

func WithWindowAuthParent(ctx context.Context, parent string) context.Context {
	return context.WithValue(ctx, windowAuthParentKey{}, parent)
}

func WithWindowAuthPolicy(ctx context.Context, policy WindowAuthPolicy) context.Context {
	return context.WithValue(ctx, windowAuthPolicyKey{}, policy)
}

func WithWindowAuthAccountPolicy(ctx context.Context, policy WindowAuthAccountPolicy) context.Context {
	return context.WithValue(ctx, windowAuthAccountPolicyKey{}, policy)
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
	manager            *Manager
	lease              *Lease
	record             windowAuthRecord
	ctx                context.Context
	cancel             context.CancelFunc
	policy             WindowAuthPolicy
	tasks              chan *windowAuthTask
	done               chan struct{}
	revoked            atomic.Bool
	revocationMu       sync.Mutex
	revocationRecorded bool
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
	if options.Method == sdk.SavedSignInMethod {
		return sdk.WindowAuthFlowView{}, errors.New("saved sign-ins require the local import command")
	}
	parent, request, err := windowAuthIdentity(ctx, true)
	if err != nil {
		return sdk.WindowAuthFlowView{}, err
	}
	entry, config, grant, err := m.windowAuthGrant(ctx, id, revision, part, binding)
	if err != nil {
		return sdk.WindowAuthFlowView{}, err
	}
	config.Values, err = effectiveModuleConfig(entry.Manifest, config.Values)
	if err != nil {
		return sdk.WindowAuthFlowView{}, errors.New("window authentication configuration is unavailable")
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
	if !sameModuleConfig(lease.worker.config, config) {
		return sdk.WindowAuthFlowView{}, errors.New("window authentication configuration changed")
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
	if part == sdk.ProviderSignInPart {
		journalRevision = subscriptionJournalRevision(ctx, journalRevision)
	}
	record := windowAuthRecord{
		Flow: storeHash(nonce), Intent: storeHash(id, revision, part, parent, request),
		Fingerprint: storeHash(binding, options.Method, string(selected), compact.String()),
		Parent:      parent, Revision: journalRevision, Config: config.Version,
		Part: journalPart, Binding: binding, Method: options.Method, Scope: scope,
		Phase: "reserved", Created: expires.Add(-10 * time.Minute).UnixMilli(), Expires: expires.UnixMilli(),
	}
	if part == sdk.ProviderSignInPart && journalRevision != providerSignInRevision(entry, config.Version) {
		record.Fingerprint = storeHash(record.Fingerprint, journalRevision)
	}
	options.Input = input
	var created bool
	err = m.withWindowAuthConfig(ctx, record, lease.worker.config, func() error {
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
		if !record.active() {
			view, err := m.replayWindowAuth(ctx, record)
			return m.windowAuthResult(ctx, record.Scope, view, err)
		}
		if owner != nil {
			if !sameModuleConfig(owner.lease.worker.config, config) {
				return sdk.WindowAuthFlowView{}, errors.New("window authentication configuration changed")
			}
			view, err := owner.submit(ctx, true, options)
			return m.windowAuthResult(ctx, record.Scope, view, err)
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
	record, err := m.store.readWindowAuthRecord(ctx, id, flow, false)
	journalPart, journalRevision := windowAuthJournalSelection(entry, config.Version, part)
	if part == sdk.ProviderSignInPart {
		journalRevision = subscriptionJournalRevision(ctx, journalRevision)
	}
	if err != nil || record.Parent != parent || record.Part != journalPart || record.Binding != binding ||
		record.Revision != journalRevision || record.Config != config.Version || record.Scope.Provider != grant.Provider {
		return windowAuthRecord{}, nil, errors.New("window authentication flow does not belong to this view")
	}
	m.mu.Lock()
	owner := m.windowAuthOwners[flow]
	m.mu.Unlock()
	config = sdk.ConfigSnapshot{Version: record.Config}
	if owner != nil && record.active() {
		config = owner.lease.worker.config
	}
	if err := m.withWindowAuthConfig(ctx, record, config, nil); err != nil {
		return windowAuthRecord{}, nil, err
	}
	if owner == nil && record.active() {
		record, err = m.store.readWindowAuthRecord(ctx, id, flow, false)
		if err != nil {
			return windowAuthRecord{}, nil, errors.New("window authentication receipt is unavailable")
		}
	}
	return record, owner, nil
}

func (m *Manager) CoreAuthFinish(ctx context.Context, id, revision, part, binding, flow string) (sdk.WindowAuthFlowView, error) {
	record, owner, err := m.windowAuthFlow(ctx, id, revision, part, binding, flow)
	if err != nil {
		return sdk.WindowAuthFlowView{}, err
	}
	if owner != nil {
		view, err := owner.submit(ctx, false, sdk.WindowAuthStartOptions{})
		return m.windowAuthResult(ctx, record.Scope, view, err)
	}
	switch record.Phase {
	case "complete", "canceled", "expired", "uncertain":
		view, err := m.replayWindowAuth(ctx, record)
		return m.windowAuthResult(ctx, record.Scope, view, err)
	default:
		return sdk.WindowAuthFlowView{Flow: flow, State: "uncertain"}, nil
	}
}

func (m *Manager) replayWindowAuth(ctx context.Context, record windowAuthRecord) (sdk.WindowAuthFlowView, error) {
	if record.Phase != "complete" {
		return record.public(), nil
	}
	err := m.withWindowAuthAuthority(ctx, record, func() error {
		confirmed, err := m.store.readWindowAuthRecord(ctx, record.Scope.Plugin, record.Flow, true)
		if err != nil {
			return err
		}
		if confirmed.Phase != "complete" || confirmed.Receipt == nil || *confirmed.Receipt != *record.Receipt ||
			confirmed.Intent != record.Intent || confirmed.Fingerprint != record.Fingerprint ||
			confirmed.Parent != record.Parent || confirmed.Revision != record.Revision || confirmed.Config != record.Config ||
			confirmed.Part != record.Part || confirmed.Binding != record.Binding || confirmed.Scope != record.Scope ||
			confirmed.Canceled != record.Canceled {
			return errWindowAuthUnconfirmed
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return sdk.WindowAuthFlowView{}, ctx.Err()
		}
		return sdk.WindowAuthFlowView{}, errWindowAuthUnconfirmed
	}
	return record.public(), nil
}

func (m *Manager) windowAuthResult(ctx context.Context, scope sdk.Scope, view sdk.WindowAuthFlowView, err error) (sdk.WindowAuthFlowView, error) {
	if err == nil && view.Account != nil {
		if policy, ok := ctx.Value(windowAuthAccountPolicyKey{}).(WindowAuthAccountPolicy); ok && policy != nil {
			policy(ctx, m, scope, *view.Account)
		}
	}
	return view, err
}

func (m *Manager) CoreAuthCancel(ctx context.Context, id, revision, part, binding, flow string) (sdk.WindowAuthFlowView, error) {
	record, owner, err := m.windowAuthFlow(ctx, id, revision, part, binding, flow)
	if err != nil {
		return sdk.WindowAuthFlowView{}, err
	}
	if !record.active() {
		return m.replayWindowAuth(ctx, record)
	}
	config := sdk.ConfigSnapshot{Version: record.Config}
	if owner != nil {
		config = owner.lease.worker.config
	}
	err = m.withWindowAuthConfig(ctx, record, config, func() error {
		var updateErr error
		record, updateErr = m.store.updateWindowAuth(id, flow, func(record *windowAuthRecord, _ *storeAccounts) error {
			if record.Phase == "complete" || record.Phase == "uncertain" {
				return nil
			}
			record.Canceled = true
			if record.Phase == "waiting" || record.Phase == "reserved" {
				record.Phase, record.Continuation = "canceled", nil
			}
			return nil
		})
		return updateErr
	})
	if err != nil {
		return sdk.WindowAuthFlowView{}, errors.New("window authentication cancellation could not be recorded")
	}
	if owner != nil && record.Phase == "canceled" {
		owner.cancel()
	}
	return m.replayWindowAuth(ctx, record)
}

func (owner *windowAuthOwner) submit(ctx context.Context, start bool, options sdk.WindowAuthStartOptions) (sdk.WindowAuthFlowView, error) {
	task := &windowAuthTask{start: start, caller: ctx, options: options, done: make(chan struct{})}
	select {
	case owner.tasks <- task:
	case <-ctx.Done():
		return sdk.WindowAuthFlowView{}, ctx.Err()
	case <-owner.done:
		record, err := owner.manager.store.readWindowAuthRecord(ctx, owner.record.Scope.Plugin, owner.record.Flow, false)
		if err != nil {
			return sdk.WindowAuthFlowView{}, errors.New("window authentication receipt is unavailable")
		}
		return owner.manager.replayWindowAuth(ctx, record)
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
			if errors.Is(task.err, errWindowAuthUnconfirmed) || task.view.State == "complete" || task.view.State == "uncertain" || task.view.State == "canceled" || task.view.State == "expired" {
				return
			}
		}
	}
}

func (owner *windowAuthOwner) execute(task *windowAuthTask) (sdk.WindowAuthFlowView, error) {
	manager, plugin, flow := owner.manager, owner.record.Scope.Plugin, owner.record.Flow
	record, err := manager.store.readWindowAuthRecord(task.caller, plugin, flow, false)
	if err != nil {
		return sdk.WindowAuthFlowView{}, errors.New("window authentication state is unavailable")
	}
	if task.caller.Err() != nil {
		return sdk.WindowAuthFlowView{}, task.caller.Err()
	}
	if record.Canceled || record.Phase == "complete" || record.Phase == "canceled" || record.Phase == "expired" || record.Phase == "uncertain" {
		return manager.replayWindowAuth(task.caller, record)
	}
	if err := owner.checkRevoked(); err != nil {
		return sdk.WindowAuthFlowView{}, err
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
		return owner.withAuthority(current, record, nil)
	})
	err = owner.withActiveAuthority(ctx, record, func() error {
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
	ctx = context.WithValue(ctx, windowAuthDispatchKey{}, windowAuthDispatch{
		owner: owner, record: record, config: owner.lease.worker.config,
	})
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
				err = owner.withActiveAuthority(ctx, record, func() error {
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
				err = owner.withActiveAuthority(ctx, record, func() error {
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
				var completed windowAuthRecord
				err = manager.withWindowAuthSelection(ctx, record, func(entry Installed, from string) error {
					if err := manager.store.checkSubscriptionResult(ctx, record.Scope, result.ID); err != nil {
						return err
					}

					var err error
					completed, err = manager.store.publishWindowAuth(ctx, plugin, flow, result, func(root *os.Root) error {
						if err := manager.checkWindowAuthConfigAtRoot(ctx, root, entry, from, record, owner.lease.worker.config); err != nil {
							return err
						}
						return owner.checkRevoked()
					})
					return err
				})
				if completed.Phase == "complete" {
					notifyChanged()
					if err != nil {
						return sdk.WindowAuthFlowView{}, errWindowAuthUnconfirmed
					}
					return completed.public(), nil
				}
				if len(result.Credentials) <= sdk.MaxHostBytes && json.Valid(result.Credentials) {
					_, err := manager.store.updateWindowAuth(plugin, flow, func(current *windowAuthRecord, _ *storeAccounts) error {
						if current.Phase == "complete" {
							return errWindowAuthUnconfirmed
						}
						current.Phase, current.Private, current.Receipt = "uncertain", &result, nil
						return nil
					})
					if errors.Is(err, errWindowAuthUnconfirmed) {
						return sdk.WindowAuthFlowView{}, err
					}
				}
			}
		}
	}
	_, err = manager.store.updateWindowAuth(plugin, flow, func(current *windowAuthRecord, _ *storeAccounts) error {
		if current.Phase == "complete" {
			return errWindowAuthUnconfirmed
		}
		current.Phase = "uncertain"
		return nil
	})
	if errors.Is(err, errWindowAuthUnconfirmed) {
		return sdk.WindowAuthFlowView{}, err
	}
	return sdk.WindowAuthFlowView{Flow: flow, State: "uncertain"}, nil
}

func (m *Manager) markRevokedWindowAuthOwners(tracked map[string]bool, current map[string]moduleGeneration) []*windowAuthOwner {
	m.mu.Lock()
	owners := make([]*windowAuthOwner, 0, len(m.windowAuthOwners))
	for _, owner := range m.windowAuthOwners {
		if tracked[owner.record.Scope.Plugin] {
			owners = append(owners, owner)
		}
	}
	m.mu.Unlock()
	revoked := owners[:0]
	for _, owner := range owners {
		generation, found := current[owner.record.Scope.Plugin]
		entry, config := generation.entry, generation.config.Version
		if !owner.revoked.Load() && found && owner.lease.worker.matches(entry, generation.config) {
			if grant, ok := entry.Manifest.ResolveAuthBinding(owner.record.Binding); ok && grant.Provider == owner.record.Scope.Provider {
				part, revision := windowAuthJournalSelection(entry, config, grant.From)
				if grant.From == sdk.ProviderSignInPart {
					revision = subscriptionJournalRevision(owner.ctx, revision)
				}
				if part == owner.record.Part && revision == owner.record.Revision {
					continue
				}
			}
		}
		owner.revoked.Store(true)
		revoked = append(revoked, owner)
	}
	return revoked
}

func (owner *windowAuthOwner) recordRevocation() {
	owner.revocationMu.Lock()
	defer owner.revocationMu.Unlock()
	if owner.revocationRecorded {
		return
	}
	record, err := owner.manager.store.updateWindowAuth(owner.record.Scope.Plugin, owner.record.Flow, func(record *windowAuthRecord, _ *storeAccounts) error {
		if record.Phase == "complete" || record.Phase == "uncertain" {
			return nil
		}
		record.Canceled = true
		if record.Phase == "reserved" || record.Phase == "waiting" {
			record.Phase, record.Continuation = "canceled", nil
		}
		return nil
	})
	owner.revocationRecorded = err == nil
	if err != nil || record.Phase == "canceled" || record.Phase == "complete" || record.Phase == "uncertain" || record.Phase == "expired" {
		owner.cancel()
	}
}

func (owner *windowAuthOwner) checkRevoked() error {
	if owner != nil && owner.revoked.Load() {
		return errors.New("window authentication authority changed")
	}
	return nil
}

func (m *Manager) withWindowAuthAuthority(ctx context.Context, record windowAuthRecord, action func() error) error {
	return m.withWindowAuthConfig(ctx, record, sdk.ConfigSnapshot{Version: record.Config}, action)
}

func (owner *windowAuthOwner) withAuthority(ctx context.Context, record windowAuthRecord, action func() error) error {
	return owner.manager.withWindowAuthConfig(ctx, record, owner.lease.worker.config, action)
}

func (owner *windowAuthOwner) withActiveAuthority(ctx context.Context, record windowAuthRecord, action func() error) error {
	return owner.withAuthority(ctx, record, func() error {
		if err := owner.checkRevoked(); err != nil {
			return err
		}
		return action()
	})
}

func (m *Manager) withWindowAuthConfig(ctx context.Context, record windowAuthRecord, expected sdk.ConfigSnapshot, action func() error) error {
	return m.withWindowAuthSelection(ctx, record, func(entry Installed, from string) error {
		var config sdk.ConfigSnapshot
		var err error
		if expected.Values == nil {
			config, err = m.readModuleConfigContext(ctx, record.Scope.Plugin)
		} else {
			config, err = m.runtimeModuleConfigContext(ctx, entry)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return errors.New("window authentication selection changed")
		}
		if err := checkWindowAuthConfig(ctx, entry, from, record, config, expected); err != nil {
			return err
		}
		if action != nil {
			return action()
		}
		return nil
	})
}

func (m *Manager) withWindowAuthSelection(ctx context.Context, record windowAuthRecord, action func(Installed, string) error) error {
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
	entry, from, err := m.windowAuthEntry(root, record)
	if err != nil {
		return err
	}
	// Saved-subscription transactions use the same catalog authority but
	// do not carry a GUI flow identity.
	if record.Flow != "" {
		if err := m.store.checkSubscriptionView(ctx, record.Scope); err != nil {
			return err
		}
	}
	return action(entry, from)
}

func (m *Manager) windowAuthEntry(root *os.Root, record windowAuthRecord) (Installed, string, error) {
	view, err := m.catalog.readView(root)
	if err != nil {
		return Installed{}, "", err
	}
	entry, found := view.lookup(record.Scope.Plugin)
	if !found || entry.Disabled {
		return Installed{}, "", errors.New("window authentication package changed or is disabled")
	}
	grant, found := entry.Manifest.ResolveAuthBinding(record.Binding)
	if !found || grant.Provider != record.Scope.Provider {
		return Installed{}, "", errors.New("authentication grant changed")
	}
	if grant.From == sdk.ProviderSignInPart {
		entry = view.enabledEntry(entry)
		if entry.observerEnablementErr != nil {
			return Installed{}, "", errors.New("authentication enablement changed")
		}
	}
	return entry, grant.From, nil
}

func checkWindowAuthConfig(ctx context.Context, entry Installed, from string, record windowAuthRecord, config, expected sdk.ConfigSnapshot) error {
	part, revision := windowAuthJournalSelection(entry, config.Version, from)
	if record.Flow != "" && from == sdk.ProviderSignInPart {
		revision = subscriptionJournalRevision(ctx, revision)
	}
	if part != record.Part || revision != record.Revision || config.Version != record.Config || !sameModuleConfig(config, expected) {
		return errors.New("window authentication selection changed")
	}
	return ctx.Err()
}

func (m *Manager) checkWindowAuthDispatch(ctx context.Context, catalog, private *os.Root, dispatch windowAuthDispatch) error {
	entry, from, err := m.windowAuthEntry(catalog, dispatch.record)
	if err != nil {
		return err
	}
	return readStoreNamespace(ctx, private, entry.Manifest.ID, func(root *os.Root) error {
		if err := m.checkWindowAuthConfigAtRoot(ctx, root, entry, from, dispatch.record, dispatch.config); err != nil {
			return err
		}
		return dispatch.owner.checkRevoked()
	})
}

func (m *Manager) checkWindowAuthConfigAtRoot(ctx context.Context, root *os.Root, entry Installed, from string, record windowAuthRecord, expected sdk.ConfigSnapshot) error {
	value, err := storeReadState(root, sdk.Scope{Plugin: entry.Manifest.ID}, moduleConfigKey)
	if err != nil {
		return err
	}
	config, err := m.runtimeModuleConfigValue(entry, value.VersionedValue)
	if err != nil {
		return err
	}
	return checkWindowAuthConfig(ctx, entry, from, record, config, expected)
}
