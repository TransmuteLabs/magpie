package extension

import (
	"path/filepath"
	"slices"
	"sync"

	"github.com/yetone/magpie/internal/appdir"
)

var changes struct {
	sync.Mutex
	listeners     []func()
	subscriptions []*changeSubscription
}

type changeSubscription struct {
	mu       sync.Mutex
	listener func()
}

func SubscribeChanges(listener func()) func() {
	if listener == nil {
		return func() {}
	}
	subscription := &changeSubscription{listener: listener}
	changes.Lock()
	changes.subscriptions = append(changes.subscriptions, subscription)
	changes.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			changes.Lock()
			changes.subscriptions = slices.DeleteFunc(changes.subscriptions, func(current *changeSubscription) bool {
				return current == subscription
			})
			changes.Unlock()
			subscription.mu.Lock()
			subscription.listener = nil
			subscription.mu.Unlock()
		})
	}
}

// CatalogPath is intentionally independent of Default and never starts or
// stops a process. File-stamp readers may call it under their own locks.
func CatalogPath() string { return filepath.Join(appdir.Config(), "extensions", "catalog.json") }

func OnChange(listener func()) {
	if listener == nil {
		return
	}
	changes.Lock()
	changes.listeners = append(changes.listeners, listener)
	changes.Unlock()
}

func notifyChanged() {
	changes.Lock()
	listeners := append([]func(){}, changes.listeners...)
	subscriptions := slices.Clone(changes.subscriptions)
	changes.Unlock()
	for _, listener := range listeners {
		listener()
	}
	for _, subscription := range subscriptions {
		subscription.mu.Lock()
		if subscription.listener != nil {
			subscription.listener()
		}
		subscription.mu.Unlock()
	}
}
