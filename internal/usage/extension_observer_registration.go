package usage

import (
	"context"
	"sync"

	"github.com/yetone/magpie/internal/beat"
	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filememo"
)

var extensionLedgerOwners struct {
	sync.Mutex
	byManager map[*extensionhost.Manager]*extensionLedgerOwner
}

type extensionLedgerOwner struct {
	manager       *extensionhost.Manager
	ctx           context.Context
	cancel        context.CancelFunc
	ready         chan struct{}
	done          chan struct{}
	refs          int
	remove        func()
	removeRequest func()
}

// ServeExtensionLedgerObservers maintains the listener-owned SDK ledger and request observers.
func ServeExtensionLedgerObservers(ctx context.Context, manager *extensionhost.Manager) func() {
	if manager == nil || ctx.Err() != nil {
		return func() {}
	}
	var owner *extensionLedgerOwner
	for {
		extensionLedgerOwners.Lock()
		owner = extensionLedgerOwners.byManager[manager]
		if owner != nil && owner.refs == 0 {
			extensionLedgerOwners.Unlock()
			<-owner.done
			continue
		}
		if owner == nil {
			lifetime, cancel := context.WithCancel(context.Background())
			owner = &extensionLedgerOwner{manager: manager, ctx: lifetime, cancel: cancel,
				ready: make(chan struct{}), done: make(chan struct{})}
			if extensionLedgerOwners.byManager == nil {
				extensionLedgerOwners.byManager = make(map[*extensionhost.Manager]*extensionLedgerOwner)
			}
			extensionLedgerOwners.byManager[manager] = owner
			go owner.run()
		}
		owner.refs++
		extensionLedgerOwners.Unlock()
		break
	}
	<-owner.ready
	var once sync.Once
	release := func() {
		once.Do(func() {
			extensionLedgerOwners.Lock()
			owner.refs--
			last := owner.refs == 0
			if last {
				owner.cancel()
			}
			extensionLedgerOwners.Unlock()
			if last {
				<-owner.done
			}
		})
	}
	detach := context.AfterFunc(ctx, release)
	return func() {
		detach()
		release()
	}
}

func (owner *extensionLedgerOwner) run() {
	defer func() {
		if owner.remove != nil {
			owner.remove()
		}
		if owner.removeRequest != nil {
			owner.removeRequest()
		}
		extensionLedgerOwners.Lock()
		delete(extensionLedgerOwners.byManager, owner.manager)
		extensionLedgerOwners.Unlock()
		close(owner.done)
	}()
	written := filememo.Written()
	owner.reconcile()
	close(owner.ready)
	for {
		select {
		case <-owner.ctx.Done():
			return
		case <-written:
			written = filememo.Written()
		case <-beat.Next(filememo.OthersEvery()):
		}
		owner.reconcile()
	}
}

func (owner *extensionLedgerOwner) reconcile() {
	ledger, err := owner.manager.LedgerTargets()
	reconcileObserver(&owner.remove, err == nil && len(ledger) > 0, func() Observer {
		return &extensionLedgerObserver{manager: owner.manager}
	})
	requests, err := owner.manager.RequestObservationTargets()
	reconcileObserver(&owner.removeRequest, err == nil && len(requests) > 0, func() Observer {
		return &extensionRequestObserver{extensionLedgerObserver: extensionLedgerObserver{manager: owner.manager, request: true}}
	})
}

func reconcileObserver(remove *func(), needed bool, newObserver func() Observer) {
	if needed && *remove == nil {
		*remove = Observe(newObserver())
	} else if !needed && *remove != nil {
		(*remove)()
		*remove = nil
	}
}
