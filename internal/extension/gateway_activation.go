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
)

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
}

type activationRetry struct {
	identity string
	next     time.Time
	delay    time.Duration
}

type activationLease struct {
	*Lease
	stopWake func() bool
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
	return m.beginGatewayActivation(ctx, fsnotify.NewWatcher)
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
	lifetime, cancel := context.WithCancel(ctx)
	owner := &GatewayActivation{
		manager: m, ctx: lifetime, cancel: cancel, done: make(chan struct{}),
		changed: make(chan struct{}, 1), leases: map[string]activationLease{}, retries: map[string]activationRetry{},
	}
	m.gatewayActivation = owner
	m.activationWG.Add(1)
	stopping := m.startCtx
	m.mu.Unlock()
	owner.unsubscribe = SubscribeChanges(owner.wake)
	go owner.run(stopping, openWatch)
	return owner, nil
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
		for _, lease := range owner.leases {
			owner.withdraw(lease)
		}
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
	if watch.backend != nil {
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
	for id, lease := range owner.leases {
		generation, exists := desired[id]
		if !exists || !generation.entry.Manifest.ActivatesGateway() ||
			!lease.worker.matches(generation.entry, generation.config) || !lease.client.alive() {
			owner.withdraw(lease)
			delete(owner.leases, id)
		}
	}
	if err != nil || owner.ctx.Err() != nil {
		return
	}
	for id, generation := range desired {
		entry, config := generation.entry, generation.config.Version
		if !entry.Manifest.ActivatesGateway() || owner.leases[id].Lease != nil {
			continue
		}
		identity := entry.Revision + "\x00" + entry.EnableGeneration + "\x00" + config
		retry, failed := owner.retries[id]
		if failed {
			if retry.identity != identity {
				delete(owner.retries, id)
				retry = activationRetry{}
			} else if time.Now().Before(retry.next) {
				continue
			}
		}
		ctx, cancel := context.WithTimeout(owner.ctx, 10*time.Second)
		worker, client, acquireErr := manager.acquire(ctx, id, &expectedGeneration{
			packageRevision: entry.Revision, configVersion: config, enableGeneration: &entry.EnableGeneration,
		})
		cancel()
		if acquireErr != nil {
			if owner.ctx.Err() != nil {
				return
			}
			delay := min(max(retry.delay*2, gatewayActivationRetryStart), gatewayActivationRetryMax)
			owner.retries[id] = activationRetry{identity: identity, next: time.Now().Add(delay), delay: delay}
			log.Printf("extension gateway activation %s: %v", id, acquireErr)
			continue
		}
		lifetime, revoke := context.WithCancel(owner.ctx)
		lease := activationLease{
			Lease:    &Lease{manager: manager, worker: worker, client: client, group: client.group, ctx: lifetime, cancel: revoke},
			stopWake: context.AfterFunc(client.lifetime, owner.wake),
		}
		if lease.CheckCurrent(owner.ctx) != nil {
			owner.withdraw(lease)
			continue
		}
		delete(owner.retries, id)
		owner.leases[id] = lease
	}
	for id := range owner.retries {
		if generation, exists := desired[id]; !exists || !generation.entry.Manifest.ActivatesGateway() {
			delete(owner.retries, id)
		}
	}
}

func (owner *GatewayActivation) withdraw(lease activationLease) {
	lease.stopWake()
	lease.Close()
	manager := owner.manager
	manager.mu.Lock()
	closed := manager.closed
	retire := !closed && manager.workers[lease.Manifest().ID] == lease.worker
	if retire {
		manager.detachWorkerLocked(lease.worker.entry.Manifest.ID, lease.worker)
	}
	manager.mu.Unlock()
	if retire {
		manager.retire(lease.worker)
	}
	if closed {
		return
	}
	timer := time.NewTimer(35 * time.Second)
	defer timer.Stop()
	select {
	case <-lease.client.done:
	case <-manager.startCtx.Done():
	case <-timer.C:
		_ = closeModuleClient(lease.client)
	}
}
