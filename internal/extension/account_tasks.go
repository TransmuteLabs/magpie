package extension

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type AccountTaskSelection struct {
	Scope    sdk.Scope
	Revision string
	Config   string
	Model    string
	Bytes    int64
}

type AccountTaskAdmission struct {
	Context context.Context
	Scope   sdk.Scope
	Check   func(context.Context) error
	Release func()
}

type AccountTaskAdmit func(context.Context, AccountTaskSelection) (AccountTaskAdmission, error)

type accountTaskAdmitKey struct{}
type accountTaskParentKey struct{}
type accountTaskExecutionKey struct{}

type accountTaskOrigin struct {
	caller string
	admit  AccountTaskAdmit
}

type accountTaskClaim struct {
	references int
}

type accountTaskParent struct {
	owner     *AccountTasks
	selection AccountTaskSelection
	admit     AccountTaskAdmit
	caller    string
	claims    map[string]*accountTaskClaim
	done      chan struct{}
	completed bool
}

type accountTaskExecution struct {
	parent  *accountTaskParent
	scope   sdk.Scope
	task    sdk.AccountTaskDescriptor
	request sdk.AccountTaskScheduleRequest
	check   func(context.Context) error
}

type scheduledAccountTask struct {
	parent  *accountTaskParent
	request sdk.AccountTaskScheduleRequest
	scope   sdk.Scope
	pin     string
	lane    string
	claim   *accountTaskClaim
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
}

type AccountTasks struct {
	manager *Manager
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	closed  bool
	claims  map[string]*accountTaskClaim
	tasks   map[string]*scheduledAccountTask
	wg      sync.WaitGroup
}

func WithAccountTaskAdmission(ctx context.Context, caller string, admit AccountTaskAdmit) context.Context {
	return context.WithValue(ctx, accountTaskAdmitKey{}, accountTaskOrigin{caller: caller, admit: admit})
}

func (manager *Manager) BeginAccountTasks(ctx context.Context) (*AccountTasks, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed || manager.accountTasks != nil {
		return nil, errors.New("account tasks require an unowned live gateway")
	}
	manager.startLifecycleLocked()
	lifetime, cancel := context.WithCancel(ctx)
	owner := &AccountTasks{manager: manager, ctx: lifetime, cancel: cancel, done: make(chan struct{}),
		claims: map[string]*accountTaskClaim{}, tasks: map[string]*scheduledAccountTask{}}
	manager.accountTasks = owner
	manager.accountTasksWG.Add(1)
	stop := context.AfterFunc(manager.startCtx, cancel)
	context.AfterFunc(lifetime, func() {
		stop()
		owner.mu.Lock()
		owner.closed = true
		for _, task := range owner.tasks {
			task.cancel()
		}
		owner.mu.Unlock()
		owner.wg.Wait()
		manager.mu.Lock()
		if manager.accountTasks == owner {
			manager.accountTasks = nil
		}
		manager.mu.Unlock()
		close(owner.done)
		manager.accountTasksWG.Done()
	})
	return owner, nil
}

func (owner *AccountTasks) Close(ctx context.Context) error {
	owner.cancel()
	select {
	case <-owner.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func accountTaskLane(scope sdk.Scope) string {
	return scope.Plugin + "\x00" + scope.Provider + "\x00" + scope.Account
}

func (parent *accountTaskParent) lane(task, key string) string {
	scope := parent.selection.Scope
	raw, _ := json.Marshal([]string{scope.Plugin, scope.Provider, scope.Account, parent.caller, task, key})
	return string(raw)
}

func (owner *AccountTasks) releaseClaimLocked(lane string, claim *accountTaskClaim) {
	claim.references--
	if claim.references == 0 && owner.claims[lane] == claim {
		delete(owner.claims, lane)
	}
}

func (parent *accountTaskParent) finish(success bool) {
	parent.owner.mu.Lock()
	for lane, claim := range parent.claims {
		parent.owner.releaseClaimLocked(lane, claim)
	}
	parent.owner.mu.Unlock()
	parent.completed = success
	close(parent.done)
}

func (manager *Manager) beginAccountTaskForeground(ctx context.Context, lease *Lease, scope sdk.Scope, params any) (context.Context, func(bool), error) {
	manager.mu.Lock()
	owner := manager.accountTasks
	manager.mu.Unlock()
	if owner == nil || scope.Account == "" {
		return ctx, func(bool) {}, nil
	}
	owner.mu.Lock()
	closed := owner.closed
	owner.mu.Unlock()
	origin, allowed := ctx.Value(accountTaskAdmitKey{}).(accountTaskOrigin)
	if !allowed || origin.admit == nil || origin.caller == "" || closed {
		return ctx, func(bool) {}, nil
	}
	manifest := lease.worker.entry.Manifest
	if manifest.Contract.Version.Minor < 9 || !slices.ContainsFunc(manifest.Providers, func(provider sdk.ProviderDescriptor) bool {
		return provider.ID == scope.Provider && len(provider.AccountTasks) > 0
	}) {
		return ctx, func(bool) {}, nil
	}
	stamp, err := manager.store.UsageStamp(scope)
	if err != nil {
		return ctx, func(bool) {}, err
	}
	if stamp.Dirty {
		usageScope := scope
		usageScope.Operation, usageScope.Attempt, usageScope.Session = "", 0, ""
		if _, err := lease.Call(ctx, sdk.MethodProviderUsage, usageScope, sdk.UsageRequest{}, nil); err != nil {
			return ctx, func(bool) {}, err
		}
		if err := manager.store.UsageReady(scope); err != nil {
			return ctx, func(bool) {}, err
		}
	}
	raw, err := json.Marshal(params)
	var request sdk.ExecuteRequest
	if err != nil || json.Unmarshal(raw, &request) != nil || request.Model == "" {
		return ctx, func(bool) {}, errors.New("account tasks require the actual execution model")
	}
	parent := &accountTaskParent{owner: owner, done: make(chan struct{}), admit: origin.admit,
		caller: origin.caller, claims: map[string]*accountTaskClaim{},
		selection: AccountTaskSelection{Scope: scope, Revision: lease.Revision(), Config: lease.ConfigVersion(),
			Model: request.Model, Bytes: int64(len(request.Body))}}
	return context.WithValue(ctx, accountTaskParentKey{}, parent), parent.finish, nil
}

func (manager *Manager) accountTaskHostCall(ctx context.Context, scope sdk.Scope, method string, raw json.RawMessage, authority hostAuthority) (any, error) {
	parent, bound := ctx.Value(accountTaskParentKey{}).(*accountTaskParent)
	component, owned := ctx.Value(componentAuthorityKey{}).(componentAuthority)
	readOnly, _ := ctx.Value(gatewayReadOnlyKey{}).(bool)
	if !bound || !owned || component.group == nil || component.effect != "write" ||
		readOnly || parent.owner.manager != manager || parent.selection.Scope != scope ||
		authority.manifest.Contract.Version.Minor < 9 ||
		authority.method != sdk.MethodProviderExecute && ctx.Value(accountTaskExecutionKey{}) == nil {
		return nil, errors.New("account tasks require their live selected execution owner")
	}
	if method == sdk.MethodAccountTaskCancel {
		request, err := sdk.DecodeAccountTaskCancel(raw)
		if err != nil {
			return nil, err
		}
		if _, found := authority.manifest.AccountTask(scope.Provider, request.Task); !found {
			return nil, errors.New("account task was not declared")
		}
		if err := manager.checkAccountTaskPins(ctx, parent.selection); err != nil {
			return nil, err
		}
		receipt, err := manager.store.Operation(scope.Plugin, scope.Operation)
		if err != nil || receipt.Scope != scope || receipt.State != "dispatched" && receipt.State != "accepted" {
			return nil, errors.New("account task cancellation requires its live foreground or tick")
		}
		lane := parent.lane(request.Task, request.Key)
		canceled := false
		owner := parent.owner
		owner.mu.Lock()
		claim := parent.claims[lane]
		if owner.closed || ctx.Err() != nil || claim != nil && owner.claims[lane] != claim {
			owner.mu.Unlock()
			return nil, errors.New("account task conversation owner changed before cancellation")
		}
		if claim == nil {
			if authority.method != sdk.MethodProviderExecute || receipt.State != "dispatched" ||
				len(parent.claims) >= sdk.MaxAccountTasks || len(owner.claims) >= 1024 {
				owner.mu.Unlock()
				return nil, errors.New("foreground must bind its bounded task key before acceptance")
			}
			claim = &accountTaskClaim{references: 1}
			parent.claims[lane], owner.claims[lane] = claim, claim
		}
		var joining []<-chan struct{}
		for _, task := range owner.tasks {
			if task.lane == lane {
				task.cancel()
				canceled = true
				if task.scope.Operation != scope.Operation {
					joining = append(joining, task.done)
				}
			}
		}
		owner.mu.Unlock()
		for _, done := range joining {
			select {
			case <-done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return sdk.AccountTaskCancelResult{Canceled: canceled}, nil
	}
	if err := component.group.accepted(scope); err != nil {
		return nil, err
	}
	request, err := sdk.DecodeAccountTaskSchedule(raw)
	if err != nil {
		return nil, err
	}
	delay := time.Until(time.UnixMilli(request.DueAt))
	if delay > time.Hour {
		return nil, errors.New("account task due time cannot be more than one hour ahead")
	}
	if _, found := authority.manifest.AccountTask(scope.Provider, request.Task); !found {
		return nil, errors.New("account task was not declared")
	}
	if err := manager.checkAccountTaskPins(ctx, parent.selection); err != nil {
		return nil, err
	}
	lane := parent.lane(request.Task, request.Key)
	owner := parent.owner
	owner.mu.Lock()
	claim := parent.claims[lane]
	current := claim != nil && owner.claims[lane] == claim && !owner.closed
	owner.mu.Unlock()
	if !current {
		return nil, errors.New("account task key is not bound to this foreground or tick")
	}
	identity, _ := json.Marshal([]any{scope, request.Task, request.Key})
	digest := sha256.Sum256(identity)
	selected := scope
	selected.Operation, selected.Attempt = "account-task-"+hex.EncodeToString(digest[:]), 1
	pinRaw, _ := json.Marshal([]any{parent.selection, request})
	pinHash := sha256.Sum256(pinRaw)
	pin := hex.EncodeToString(pinHash[:])
	if err := manager.store.reserveAccountTask(selected, pin); err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(parent.owner.ctx)
	task := &scheduledAccountTask{parent: parent, request: request, scope: selected, pin: pin,
		lane: lane, claim: claim, ctx: lifetime, cancel: cancel, done: make(chan struct{})}
	owner.mu.Lock()
	count := 0
	for _, existing := range owner.tasks {
		if accountTaskLane(existing.scope) == accountTaskLane(scope) && existing.parent.caller == parent.caller {
			count++
			if existing.lane == lane &&
				existing.scope.Operation != scope.Operation {
				existing.cancel()
			}
		}
	}
	if owner.closed || owner.ctx.Err() != nil || owner.claims[lane] != claim ||
		count >= sdk.MaxAccountTasks || len(owner.tasks) >= 256 || ctx.Err() != nil {
		owner.mu.Unlock()
		cancel()
		_ = manager.store.finishAccountTask(selected, pin, "not-started")
		return nil, errors.New("account task owner changed or its bounded queue is full")
	}
	claim.references++
	owner.tasks[selected.Operation] = task
	owner.wg.Add(1)
	owner.mu.Unlock()
	go owner.run(task)
	return sdk.AccountTaskScheduleResult{OperationID: selected.Operation}, nil
}

func (manager *Manager) checkAccountTaskPins(ctx context.Context, selection AccountTaskSelection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := manager.List()
	if err != nil || !slices.ContainsFunc(entries, func(entry Installed) bool {
		return entry.Manifest.ID == selection.Scope.Plugin && !entry.Disabled && entry.Revision == selection.Revision
	}) {
		return ErrActionChanged
	}
	config, err := manager.ModuleConfig(ctx, selection.Scope.Plugin)
	if err != nil || config.Version != selection.Config {
		return ErrActionChanged
	}
	_, err = manager.store.RefreshedAccount(selection.Scope)
	return err
}

func (owner *AccountTasks) run(task *scheduledAccountTask) {
	defer owner.wg.Done()
	state := "not-started"
	receiptScope := task.scope
	defer func() {
		task.cancel()
		_ = owner.manager.store.finishAccountTask(receiptScope, task.pin, state)
		owner.mu.Lock()
		delete(owner.tasks, task.scope.Operation)
		owner.releaseClaimLocked(task.lane, task.claim)
		owner.mu.Unlock()
		close(task.done)
	}()
	timer := time.NewTimer(time.Until(time.UnixMilli(task.request.DueAt)))
	defer timer.Stop()
	check := time.NewTicker(100 * time.Millisecond)
	defer check.Stop()
	ready, elapsed := false, false
	parentDone := task.parent.done
	for !ready || !elapsed {
		select {
		case <-task.ctx.Done():
			return
		case <-parentDone:
			if !task.parent.completed {
				return
			}
			ready, parentDone = true, nil
		case <-timer.C:
			elapsed = true
		case <-check.C:
			if err := owner.manager.checkAccountTaskPins(task.ctx, task.parent.selection); err != nil {
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(task.ctx, 2*time.Minute)
	defer cancel()
	selection := task.parent.selection
	selection.Scope = task.scope
	admission, err := task.parent.admit(ctx, selection)
	if err != nil {
		return
	}
	if admission.Release == nil || admission.Check == nil || admission.Context == nil {
		return
	}
	defer admission.Release()
	scope := admission.Scope
	if scope.Plugin != task.scope.Plugin || scope.Provider != task.scope.Provider || scope.Account != task.scope.Account ||
		scope.Operation != task.scope.Operation || scope.Attempt != 1 || scope.Session != task.scope.Session {
		return
	}
	receiptScope = scope
	selection.Scope = scope
	lease, err := owner.manager.AcquireRevision(admission.Context, scope.Plugin, selection.Revision, selection.Config)
	if err != nil {
		return
	}
	defer lease.Close()
	descriptor, declared := lease.worker.entry.Manifest.AccountTask(scope.Provider, task.request.Task)
	if !declared {
		return
	}
	owner.mu.Lock()
	if owner.closed || owner.claims[task.lane] != task.claim || ctx.Err() != nil {
		owner.mu.Unlock()
		return
	}
	task.claim.references++
	owner.mu.Unlock()
	parent := &accountTaskParent{owner: owner, selection: selection, caller: task.parent.caller,
		claims: map[string]*accountTaskClaim{task.lane: task.claim}, admit: task.parent.admit, done: make(chan struct{})}
	execution := &accountTaskExecution{parent: parent, scope: scope, task: descriptor, request: task.request, check: admission.Check}
	ctx = context.WithValue(admission.Context, accountTaskExecutionKey{}, execution)
	ctx = context.WithValue(ctx, accountTaskParentKey{}, parent)
	monitorDone := make(chan struct{})
	monitorExited := make(chan struct{})
	defer func() { close(monitorDone); <-monitorExited }()
	go func() {
		defer close(monitorExited)
		watch := time.NewTicker(100 * time.Millisecond)
		defer watch.Stop()
		for {
			select {
			case <-monitorDone:
				return
			case <-ctx.Done():
				return
			case <-watch.C:
				if lease.CheckCurrent(ctx) != nil || admission.Check(ctx) != nil {
					task.cancel()
					return
				}
			}
		}
	}()
	_, err = lease.Call(ctx, sdk.MethodModuleCall, scope, sdk.ModuleCallRequest{Action: descriptor.Action,
		Input: mustTaskInput(sdk.AccountTaskRequest{Task: descriptor.ID, Key: task.request.Key,
			Job: task.request.Job, DueAt: task.request.DueAt, Model: selection.Model, Proxy: netproxy.Choice(ctx)})}, nil)
	parent.finish(err == nil)
	if receipt, foundErr := owner.manager.store.Operation(scope.Plugin, scope.Operation); foundErr == nil &&
		receipt.State != "reserved" {
		state = receipt.State
	}
}

func mustTaskInput(request sdk.AccountTaskRequest) json.RawMessage {
	raw, _ := json.Marshal(request)
	return raw
}

func checkLeasedAccountTask(ctx context.Context, lease *Lease, method string, scope sdk.Scope, params any) (bool, error) {
	if method != sdk.MethodModuleCall {
		return false, nil
	}
	raw, err := json.Marshal(params)
	var action sdk.ModuleCallRequest
	if err != nil || json.Unmarshal(raw, &action) != nil {
		return false, errors.New("invalid account task action")
	}
	isTask := false
	for _, provider := range lease.worker.entry.Manifest.Providers {
		for _, task := range provider.AccountTasks {
			isTask = isTask || task.Action == action.Action
		}
	}
	if !isTask {
		return false, nil
	}
	execution, owned := ctx.Value(accountTaskExecutionKey{}).(*accountTaskExecution)
	if !owned || execution.scope != scope || execution.task.Action != action.Action {
		return true, &OutcomeError{Outcome: "not-started", Cause: errors.New("paid task action requires its host-owned scheduled intent")}
	}
	request, err := sdk.DecodeAccountTaskRequest(action.Input)
	if err != nil || request.Task != execution.task.ID || request.Key != execution.request.Key ||
		request.Job != execution.request.Job || request.DueAt != execution.request.DueAt ||
		request.Model != execution.parent.selection.Model ||
		request.Proxy != netproxy.Choice(ctx) {
		return true, errors.New("paid task action differs from its selected intent")
	}
	parent := execution.parent
	lane := parent.lane(request.Task, request.Key)
	parent.owner.mu.Lock()
	current := parent.claims[lane] != nil && parent.owner.claims[lane] == parent.claims[lane]
	parent.owner.mu.Unlock()
	if !current {
		return true, errors.New("paid task conversation owner was superseded")
	}
	if err := lease.CheckCurrent(ctx); err != nil {
		return true, err
	}
	if err := execution.check(ctx); err != nil {
		return true, err
	}
	return true, netproxy.Held(ctx)
}

func (manager *Manager) finishAccountTaskCall(ctx context.Context, scope sdk.Scope, manifest sdk.Manifest, raw json.RawMessage, accepted bool) error {
	result, err := sdk.DecodeAccountTaskResult(manifest, raw)
	if err != nil {
		return err
	}
	if result.Call != nil {
		payload, _ := json.Marshal(sdk.OwnCallRequest{Call: *result.Call})
		if _, err := manager.recordOwnCall(ctx, scope, payload,
			hostAuthority{manifest: manifest, method: sdk.MethodModuleCall}, true); err != nil {
			return err
		}
	}
	if result.Outcome == "not-started" && !accepted {
		return &OutcomeError{Outcome: "not-started", Cause: errors.New("account task did not start")}
	}
	if result.Outcome != "completed" || !accepted {
		return &OutcomeError{Outcome: "uncertain", Cause: errors.New("account task lacks a completed accepted result")}
	}
	parent, found := ctx.Value(ownCallParentKey{}).(*ownCallParent)
	if !found {
		return errors.New("account task lost its accounting owner")
	}
	parent.mu.Lock()
	measured := parent.snapshot != nil
	parent.mu.Unlock()
	if !measured {
		return fmt.Errorf("completed paid account task omitted normal accounting evidence")
	}
	return nil
}

func (manager *Manager) captureAccountTaskUsage(ctx context.Context, scope sdk.Scope, manifest sdk.Manifest, raw json.RawMessage) error {
	var call sdk.OwnCall
	if !decodeHostParams(raw, &call) {
		return errors.New("invalid paid task usage snapshot")
	}
	payload, err := json.Marshal(sdk.OwnCallRequest{Call: call})
	if err != nil {
		return err
	}
	_, err = manager.recordOwnCall(ctx, scope, payload,
		hostAuthority{manifest: manifest, method: sdk.MethodModuleCall}, true)
	return err
}

func (manager *Manager) flushAccountTaskUsage(ctx context.Context, scope sdk.Scope, terminal error) error {
	task, owned := ctx.Value(accountTaskExecutionKey{}).(*accountTaskExecution)
	parent, live := ctx.Value(ownCallParentKey{}).(*ownCallParent)
	if !owned || !live || task.scope != scope || parent.method != sdk.MethodModuleCall {
		return errors.New("paid task accounting lost its original execution owner")
	}
	parent.mu.Lock()
	if parent.appended || parent.snapshot == nil {
		parent.mu.Unlock()
		return nil
	}
	ownCalls.RLock()
	handler := ownCalls.handler
	ownCalls.RUnlock()
	if handler == nil {
		parent.mu.Unlock()
		return errors.New("ordinary usage accounting is unavailable")
	}
	call := *parent.snapshot
	call.Usage = *call.Usage.Clone()
	if terminal != nil && call.Error == "" {
		call.Error = "account task outcome uncertain"
	}
	parent.appended = true
	parent.mu.Unlock()
	handler(context.WithoutCancel(ctx), scope, call)
	return nil
}
