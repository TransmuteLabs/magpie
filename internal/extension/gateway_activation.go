package extension

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	gatewayActivationFallback   = 5 * time.Second
	gatewayActivationCoalesce   = 20 * time.Millisecond
	gatewayActivationRetryStart = time.Second
	gatewayActivationRetryMax   = time.Minute
	gatewayActivationStable     = time.Minute
	gatewayActivationStarts     = 4
)

var errActivationWorkerExited = errors.New("extension worker exited")

type GatewayActivation struct {
	manager     *Manager
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	changed     chan struct{}
	unsubscribe func()
	once        sync.Once
	leases      map[string]activationLease
	retries     map[string]activationRetry
	desired     map[string]moduleGeneration
	draining    map[string]activationLease
	starting    map[string]activationStart
	started     chan activationResult
	startWG     sync.WaitGroup
	exitWG      sync.WaitGroup
}

type activationStart struct {
	generation moduleGeneration
	cancel     context.CancelFunc
	superseded bool
}

type activationResult struct {
	id    string
	lease *Lease
	err   error
}

type activationRetry struct {
	identity string
	next     time.Time
	delay    time.Duration
}

type activationLease struct {
	*Lease
	retired  <-chan struct{}
	stopWake func()
	since    time.Time
}

// ListenerOwnerDraining identifies a canceled listener that still holds an
// SDK owner. A successor can wait for it outside the HTTP startup path.
type ListenerOwnerDraining struct {
	done <-chan struct{}
}

func (*ListenerOwnerDraining) Error() string { return "extension listener owner is draining" }

func (drain *ListenerOwnerDraining) Wait(ctx context.Context) error {
	select {
	case <-drain.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func listenerOwnerConflict(ctx context.Context, done <-chan struct{}) error {
	if ctx.Err() != nil {
		return &ListenerOwnerDraining{done: done}
	}
	return errors.New("extension owner already has a live listener")
}

func (m *Manager) BeginGatewayActivation(ctx context.Context) (*GatewayActivation, error) {
	return m.beginGatewayActivation(ctx, nil)
}

func (m *Manager) beginGatewayActivation(ctx context.Context, openWatch func() (*fsnotify.Watcher, error)) (*GatewayActivation, error) {
	m.mu.Lock()
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("gateway activation needs a live manager")
	}
	if previous := m.gatewayActivation; previous != nil {
		m.mu.Unlock()
		return nil, listenerOwnerConflict(previous.ctx, previous.done)
	}
	m.startLifecycleLocked()
	owner := newGatewayActivation(m, ctx)
	m.gatewayActivation = owner
	m.activationWG.Add(1)
	stopping := m.startCtx
	m.mu.Unlock()
	owner.unsubscribe = SubscribeChanges(owner.wake)
	go owner.run(stopping, openWatch)
	return owner, nil
}

func newGatewayActivation(manager *Manager, ctx context.Context) *GatewayActivation {
	ctx, cancel := context.WithCancel(ctx)
	return &GatewayActivation{
		manager: manager, ctx: ctx, cancel: cancel, done: make(chan struct{}),
		changed: make(chan struct{}, 1), leases: map[string]activationLease{}, retries: map[string]activationRetry{},
		draining: map[string]activationLease{}, starting: map[string]activationStart{},
		started: make(chan activationResult, gatewayActivationStarts),
	}
}

func (owner *GatewayActivation) wake() {
	select {
	case owner.changed <- struct{}{}:
	default:
	}
}

func (owner *GatewayActivation) Close(ctx context.Context) error {
	owner.once.Do(owner.cancel)
	select {
	case <-owner.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (owner *GatewayActivation) run(stopping context.Context, openWatch func() (*fsnotify.Watcher, error)) {
	defer func() {
		owner.cancel()
		owner.unsubscribe()
		owner.shutdown()
		owner.manager.mu.Lock()
		if owner.manager.gatewayActivation == owner {
			owner.manager.gatewayActivation = nil
		}
		owner.manager.mu.Unlock()
		close(owner.done)
		owner.manager.activationWG.Done()
	}()
	stop := context.AfterFunc(stopping, owner.cancel)
	defer stop()
	watch := newActivationWatch(owner.manager.root)
	defer watch.close()
	watch.open(openWatch)
	sweep := time.NewTicker(time.Minute)
	defer sweep.Stop()
	fallback := time.NewTicker(gatewayActivationFallback)
	defer fallback.Stop()
	debounce := time.NewTimer(gatewayActivationCoalesce)
	defer debounce.Stop()
	debounce.Stop()
	var pending <-chan time.Time
	schedule := func() {
		if pending == nil {
			debounce.Reset(gatewayActivationCoalesce)
			pending = debounce.C
		}
	}
	owner.reapSavedSignIns()
	owner.reconcile(watch)
	for owner.ctx.Err() == nil {
		select {
		case <-owner.ctx.Done():
			return
		case <-owner.changed:
			schedule()
		case result := <-owner.started:
			owner.finishStart(result)
			owner.startReady()
		case event, open := <-watch.events():
			if !open {
				watch.close()
				schedule()
			} else if watch.relevant(event) {
				schedule()
			}
		case <-watch.errors():
			watch.close()
			schedule()
		case _, open := <-watch.vnodeChanges():
			if !open {
				watch.close()
				schedule()
			} else if watch.vnodeRelevant() {
				schedule()
			}
		case <-pending:
			pending = nil
			owner.reconcile(watch)
		case <-fallback.C:
			debounce.Stop()
			pending = nil
			watch.open(openWatch)
			owner.reconcile(watch)
		case <-sweep.C:
			owner.reapSavedSignIns()
		}
	}
}

func (owner *GatewayActivation) reapSavedSignIns() {
	ctx, cancel := context.WithTimeout(owner.ctx, time.Second)
	defer cancel()
	if err := owner.manager.store.reapSavedSignIns(ctx); err != nil && owner.ctx.Err() == nil {
		log.Printf("extension saved sign-in cleanup: %v", err)
	}
}

func (owner *GatewayActivation) reconcile(watch *activationWatch) {
	manager := owner.manager
	watch.begin()
	var modules []string
	var observe func(string)
	if watch.active() {
		observe = func(id string) {
			if len(modules) < activationWatchLimit-1 {
				modules = append(modules, id)
			}
		}
	}
	desired, err := manager.reconcile(owner.ctx, true, observe)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	for _, id := range modules {
		watch.followConfig(id)
	}
	if watch.finish() {
		owner.wake()
	}
	owner.desired = desired
	if err != nil {
		owner.desired = nil
	}
	for id, start := range owner.starting {
		if generation, exists := desired[id]; !exists || !sameActivationGeneration(generation, start.generation) {
			start.cancel()
			start.superseded = true
			owner.starting[id] = start
		}
	}
	for id, lease := range owner.leases {
		generation, exists := desired[id]
		current := exists && generation.entry.Manifest.ActivatesGateway() &&
			lease.worker.matches(generation.entry, generation.config)
		if current && lease.client.alive() {
			if time.Since(lease.since) >= gatewayActivationStable {
				delete(owner.retries, id)
			}
			continue
		}
		owner.withdraw(lease)
		delete(owner.leases, id)
		if current {
			owner.recordFailure(id, activationIdentity(generation.entry, generation.config.Version), errActivationWorkerExited)
		}
	}
	if err != nil {
		return
	}
	for id := range owner.retries {
		if generation, exists := desired[id]; !exists || !generation.entry.Manifest.ActivatesGateway() {
			delete(owner.retries, id)
		}
	}
	if len(owner.leases) == len(desired) && len(owner.starting) == 0 &&
		len(owner.draining) == 0 && len(owner.retries) == 0 {
		return
	}
	owner.startReady()
}

func sameActivationGeneration(current, expected moduleGeneration) bool {
	return current.entry.Manifest.ActivatesGateway() && current.entry.Revision == expected.entry.Revision &&
		current.entry.Directory == expected.entry.Directory &&
		current.entry.EnableGeneration == expected.entry.EnableGeneration && sameModuleConfig(current.config, expected.config)
}

func (owner *GatewayActivation) startReady() {
	for id, lease := range owner.draining {
		select {
		case <-lease.retired:
			lease.stopWake()
			delete(owner.draining, id)
		default:
		}
	}
	if owner.ctx.Err() != nil {
		return
	}
	for id, generation := range owner.desired {
		if len(owner.starting) == gatewayActivationStarts {
			return
		}
		entry, config := generation.entry, generation.config.Version
		if !entry.Manifest.ActivatesGateway() || owner.leases[id].Lease != nil ||
			owner.draining[id].Lease != nil || owner.starting[id].cancel != nil {
			continue
		}
		identity := activationIdentity(entry, config)
		if retry, failed := owner.retries[id]; failed {
			if retry.identity != identity {
				delete(owner.retries, id)
			} else if time.Now().Before(retry.next) {
				continue
			}
		}
		ctx, cancel := context.WithTimeout(owner.ctx, 10*time.Second)
		owner.starting[id] = activationStart{generation: generation, cancel: cancel}
		owner.startWG.Go(func() {
			defer cancel()
			lease, err := owner.start(ctx, generation)
			owner.started <- activationResult{id: id, lease: lease, err: err}
		})
	}
}

func (owner *GatewayActivation) start(ctx context.Context, generation moduleGeneration) (*Lease, error) {
	manager := owner.manager
	entry := generation.entry
	worker, err := manager.selectGenerationWorker(ctx, entry.Manifest.ID, &expectedGeneration{
		packageRevision: entry.Revision, configVersion: generation.config.Version,
		enableGeneration: &entry.EnableGeneration, exact: true,
	})
	if err != nil {
		return nil, err
	}
	if !worker.matches(entry, generation.config) {
		return nil, errors.New("extension configuration changed before activation")
	}
	worker, client, err := manager.acquireWorker(ctx, worker, true)
	if err != nil {
		return nil, err
	}
	lease := manager.newLease(owner.ctx, worker, client)
	return lease, lease.CheckCurrent(ctx)
}

func (owner *GatewayActivation) finishStart(result activationResult) {
	start := owner.starting[result.id]
	delete(owner.starting, result.id)
	generation, exists := owner.desired[result.id]
	current := owner.ctx.Err() == nil && !start.superseded && exists && sameActivationGeneration(generation, start.generation)
	if result.lease != nil {
		lease := owner.watchWorker(result.lease)
		if current && result.err == nil && lease.client.alive() {
			lease.since = time.Now()
			owner.leases[result.id] = lease
			return
		}
		owner.withdraw(lease)
		if result.err == nil {
			result.err = errActivationWorkerExited
		}
	}
	if current && result.err != nil {
		owner.recordFailure(result.id, activationIdentity(generation.entry, generation.config.Version), result.err)
	}
}

func activationIdentity(entry Installed, config string) string {
	return entry.Revision + "\x00" + entry.EnableGeneration + "\x00" + config
}

func (owner *GatewayActivation) recordFailure(id, identity string, err error) {
	if owner.ctx.Err() != nil {
		return
	}
	delay := gatewayActivationRetryStart
	if retry := owner.retries[id]; retry.identity == identity {
		delay = min(retry.delay*2, gatewayActivationRetryMax)
	}
	owner.retries[id] = activationRetry{identity: identity, next: time.Now().Add(delay), delay: delay}
	log.Printf("extension gateway activation %s: %v", id, err)
}

func (owner *GatewayActivation) watchWorker(lease *Lease) activationLease {
	retired := lease.client.done
	if group := lease.client.group; group != nil {
		retired = group.closeDone
	}
	owner.exitWG.Add(1)
	stop := context.AfterFunc(lease.client.lifetime, func() {
		defer owner.exitWG.Done()
		owner.wake()
		select {
		case <-retired:
			owner.wake()
		case <-owner.ctx.Done():
		}
	})
	return activationLease{Lease: lease, retired: retired, stopWake: func() {
		if stop() {
			owner.exitWG.Done()
		}
	}}
}

func (owner *GatewayActivation) withdraw(lease activationLease) {
	lease.Close()
	manager := owner.manager
	id := lease.worker.entry.Manifest.ID
	manager.mu.Lock()
	closed := manager.closed
	retire := !closed && manager.workers[id] == lease.worker &&
		manager.detachWorkerLocked(id, lease.worker, lease.client)
	manager.mu.Unlock()
	if retire {
		manager.retire(lease.worker)
	}
	if closed {
		lease.stopWake()
		return
	}
	select {
	case <-lease.retired:
		lease.stopWake()
	default:
		owner.draining[id] = lease
	}
}

func (owner *GatewayActivation) shutdown() {
	owner.cancel()
	owner.startWG.Wait()
	for len(owner.starting) != 0 {
		owner.finishStart(<-owner.started)
	}
	for _, lease := range owner.leases {
		owner.withdraw(lease)
	}
	for _, lease := range owner.draining {
		select {
		case <-lease.retired:
		case <-owner.manager.startCtx.Done():
		}
		lease.stopWake()
	}
	owner.exitWG.Wait()
}
