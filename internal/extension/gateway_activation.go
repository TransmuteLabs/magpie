package extension

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

type GatewayActivation struct {
	manager     *Manager
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	changed     chan struct{}
	unsubscribe func()
	once        sync.Once
	leases      map[string]*Lease
	failed      map[string]string
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
		changed: make(chan struct{}, 1), leases: map[string]*Lease{}, failed: map[string]string{},
	}
	m.gatewayActivation = owner
	m.activationWG.Add(1)
	stopping := m.startCtx
	m.mu.Unlock()
	owner.unsubscribe = SubscribeChanges(func() {
		select {
		case owner.changed <- struct{}{}:
		default:
		}
	})
	go owner.run(stopping)
	return owner, nil
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

func (owner *GatewayActivation) run(stopping context.Context) {
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
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var nextSignInSweep time.Time
	for {
		if time.Now().After(nextSignInSweep) {
			nextSignInSweep = time.Now().Add(time.Minute)
			ctx, cancel := context.WithTimeout(owner.ctx, time.Second)
			err := owner.manager.store.reapSavedSignIns(ctx)
			cancel()
			if err != nil && owner.ctx.Err() == nil {
				log.Printf("extension saved sign-in cleanup: %v", err)
			}
		}
		owner.reconcile()
		select {
		case <-owner.ctx.Done():
			return
		case <-owner.changed:
		case <-ticker.C:
		}
	}
}

func (owner *GatewayActivation) reconcile() {
	manager := owner.manager
	desired, err := manager.reconcile(owner.ctx, true)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
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
		if !entry.Manifest.ActivatesGateway() || owner.leases[id] != nil {
			continue
		}
		identity := entry.Revision + "\x00" + entry.EnableGeneration + "\x00" + config
		if owner.failed[id] == identity {
			continue
		}
		ctx, cancel := context.WithTimeout(owner.ctx, 10*time.Second)
		worker, client, acquireErr := manager.acquire(ctx, id, &expectedGeneration{
			packageRevision: entry.Revision, configVersion: config, enableGeneration: &entry.EnableGeneration,
		})
		cancel()
		if acquireErr != nil {
			owner.failed[id] = identity
			if owner.ctx.Err() == nil {
				log.Printf("extension gateway activation %s: %v", id, acquireErr)
			}
			continue
		}
		lifetime, revoke := context.WithCancel(owner.ctx)
		lease := &Lease{manager: manager, worker: worker, client: client, group: client.group, ctx: lifetime, cancel: revoke}
		if lease.CheckCurrent(owner.ctx) != nil {
			owner.withdraw(lease)
			continue
		}
		delete(owner.failed, id)
		owner.leases[id] = lease
	}
	for id := range owner.failed {
		if generation, exists := desired[id]; !exists || !generation.entry.Manifest.ActivatesGateway() {
			delete(owner.failed, id)
		}
	}
}

func (owner *GatewayActivation) withdraw(lease *Lease) {
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
