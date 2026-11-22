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
	"cmp"
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type Seat = sdk.LinkedRoutingSeat
type Strategy = sdk.LinkedRoutingStrategy

type Option struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Plugin string `json:"plugin"`
}

type Source struct {
	Find func(string) (Strategy, bool)
	List func(context.Context) []Option
}

type registration struct {
	option   Option
	strategy Strategy
}

type registrations struct {
	sync.RWMutex
	byID     map[string][]*registration
	order    []*registration
	rotation rotationState
}

// Catalog resolves and lists strategies for one application. Linked and
// legacy registrations share the built-in runtime's lifetime.
type Catalog struct {
	registered *registrations
	installed  Source
}

func NewCatalog() *Catalog {
	smart := &registration{option: Option{Name: "Quota first"}, strategy: Smart}
	return &Catalog{registered: &registrations{
		byID:  map[string][]*registration{"": {smart}},
		order: []*registration{smart},
		rotation: rotationState{
			rounds: make(map[string]*round),
			turns:  make(map[string]int),
			probes: make(map[string]probe),
			drains: make(map[string]drainAt),
		},
	}}
}

// WithSource binds this runtime's registrations to a gateway's installed
// module source. Existing gateways retain their own binding when replaced.
func (c *Catalog) WithSource(installed Source) *Catalog {
	return &Catalog{registered: c.registered, installed: installed}
}

// Register serves option until remove is called. The most recent
// registration of an ID takes precedence over the installed source.
func (c *Catalog) Register(option Option, strategy Strategy) (remove func()) {
	if option.ID == "" || strategy.Order == nil {
		panic("routes: invalid strategy registration")
	}
	return c.register([]*registration{{option: option, strategy: strategy}})
}

func (c *Catalog) RegisterLinked(declared []sdk.LinkedRoutingRegistration) (func(), error) {
	entries := make([]*registration, 0, len(declared))
	seen := make(map[string]bool, len(declared))
	for _, binding := range declared {
		if err := binding.Strategy.Validate(binding.ID); err != nil {
			return nil, err
		}
		if seen[binding.ID] {
			return nil, fmt.Errorf("duplicate linked routing strategy %q", binding.ID)
		}
		seen[binding.ID] = true
		entries = append(entries, &registration{
			option: Option{ID: binding.ID, Name: cmp.Or(binding.Name, binding.ID)}, strategy: binding.Strategy,
		})
	}
	return c.register(entries), nil
}

func (c *Catalog) register(entries []*registration) func() {
	reg := c.registered
	reg.Lock()
	for _, entry := range entries {
		id := entry.option.ID
		reg.byID[id] = append(reg.byID[id], entry)
	}
	reg.order = append(reg.order, entries...)
	reg.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			reg.Lock()
			defer reg.Unlock()
			for _, entry := range entries {
				same := func(candidate *registration) bool { return candidate == entry }
				id := entry.option.ID
				stack := slices.DeleteFunc(reg.byID[id], same)
				if len(stack) == 0 {
					delete(reg.byID, id)
				} else {
					reg.byID[id] = stack
				}
			}
			reg.order = slices.DeleteFunc(reg.order, func(entry *registration) bool { return slices.Contains(entries, entry) })
		})
	}
}

func (c *Catalog) Find(routing string) (Strategy, bool) {
	reg := c.registered
	reg.RLock()
	stack := reg.byID[routing]
	if len(stack) != 0 {
		strategy := stack[len(stack)-1].strategy
		reg.RUnlock()
		return strategy, true
	}
	reg.RUnlock()
	if c.installed.Find != nil && strings.Contains(routing, "/") {
		if strategy, found := safeLookup(c.installed.Find, routing); found && strategy.Order != nil {
			return strategy, true
		}
	}
	return Strategy{}, false
}

func (c *Catalog) Served(routing string) bool {
	_, ok := c.Find(routing)
	return ok
}

// List returns the serving registrations followed by the installed metadata.
// Reading the installed source never starts a worker.
func (c *Catalog) List() []Option {
	return c.ListContext(context.Background())
}

func (c *Catalog) ListContext(ctx context.Context) []Option {
	reg := c.registered
	reg.RLock()
	options := make([]Option, 0, len(reg.byID))
	for _, entry := range reg.order {
		stack := reg.byID[entry.option.ID]
		if stack[len(stack)-1] == entry {
			options = append(options, entry.option)
		}
	}
	reg.RUnlock()
	if c.installed.List == nil {
		return options
	}
	seen := make(map[string]bool, len(options))
	for _, option := range options {
		seen[option.ID] = true
	}
	for _, option := range safeList(ctx, c.installed.List) {
		if strings.Contains(option.ID, "/") && !seen[option.ID] {
			seen[option.ID] = true
			options = append(options, option)
		}
	}
	return options
}

func safeLookup(find func(string) (Strategy, bool), name string) (strategy Strategy, found bool) {
	defer func() {
		if recover() != nil {
			strategy, found = Strategy{}, false
		}
	}()
	return find(name)
}

func safeList(ctx context.Context, list func(context.Context) []Option) (out []Option) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	return list(ctx)
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
