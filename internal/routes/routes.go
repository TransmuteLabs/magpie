// Package routes is the gateway's router strategy point (P3 of
// docs/subsystems/plugin-runtime.md): how one pool of routes — a
// provider's accounts or keys, or the heads of a group's members — is
// ordered for a request. The gateway lists the pool, rests, affinity and
// failover itself; a Strategy only says the order.
//
// Smart, the default, is the core's. The others a provider or group can be
// routed by (order, rotate, usage, pace, weight, free, fast, cheap,
// drain, expiring) are here for a plugin to
// register: plugins/rotation does, and off, a pool routed by one of them is
// tried in its listed order, as an unknown routing always was.
package routes

import (
	"log"
	"slices"
	"sync"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type Seat = sdk.LinkedRoutingSeat
type Strategy = sdk.LinkedRoutingStrategy

var reg = struct {
	sync.RWMutex
	m map[string][]*Strategy // routing → registered, the last one serving
}{m: map[string][]*Strategy{}}

// Register serves routing by s until remove is called. Registering one
// already served stacks: the last serves until it is removed.
func Register(routing string, s Strategy) (remove func()) {
	e := &s
	reg.Lock()
	reg.m[routing] = append(reg.m[routing], e)
	reg.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			reg.Lock()
			defer reg.Unlock()
			reg.m[routing] = slices.DeleteFunc(slices.Clone(reg.m[routing]), func(x *Strategy) bool { return x == e })
		})
	}
}

// Find is the strategy serving routing: Smart for "", else the last one
// registered; ok is false when none is.
func Find(routing string) (Strategy, bool) {
	if routing == "" {
		return Smart, true
	}
	reg.RLock()
	l := reg.m[routing]
	if len(l) == 0 {
		reg.RUnlock()
		return discoveredStrategy(routing)
	}
	strategy := *l[len(l)-1]
	reg.RUnlock()
	return strategy, true
}

// Served says whether routing is served now.
func Served(routing string) bool {
	_, ok := Find(routing)
	return ok
}

// Apply is the order s gives seats, checked: an order that isn't a
// permutation of them, or a strategy that panics, leaves them in their
// listed order, logged. A strategy only orders, so failing it fails open:
// every route is still tried.
func Apply(s Strategy, pool string, seats []Seat) (order []int) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("routes: %s's strategy panicked: %v", pool, r)
			order = identity(len(seats))
		}
	}()
	order = s.Order(pool, seats)
	seen := make([]bool, len(seats))
	ok := len(order) == len(seats)
	for _, i := range order {
		if !ok || i < 0 || i >= len(seats) || seen[i] {
			ok = false
			break
		}
		seen[i] = true
	}
	if !ok {
		log.Printf("routes: %s's strategy gave %v for %d routes; trying them in order", pool, order, len(seats))
		return identity(len(seats))
	}
	return order
}
