package routes

import (
	"slices"
	"strings"
	"sync"
)

type DiscoveredRouting struct {
	ID     string
	Name   string
	Plugin string
}

type Lookup struct {
	Find func(string) (Strategy, bool)
	List func() []DiscoveredRouting
}

var lookups struct {
	sync.RWMutex
	entries []*Lookup
}

func RegisterLookup(lookup Lookup) func() {
	entry := &lookup
	lookups.Lock()
	lookups.entries = append(lookups.entries, entry)
	lookups.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			lookups.Lock()
			lookups.entries = slices.DeleteFunc(slices.Clone(lookups.entries), func(candidate *Lookup) bool { return candidate == entry })
			lookups.Unlock()
		})
	}
}

func lookupSnapshot() []*Lookup {
	lookups.RLock()
	defer lookups.RUnlock()
	return slices.Clone(lookups.entries)
}

func discoveredStrategy(name string) (Strategy, bool) {
	if !strings.Contains(name, "/") {
		return Strategy{}, false
	}
	for _, lookup := range slices.Backward(lookupSnapshot()) {
		if lookup.Find == nil {
			continue
		}
		if strategy, found := safeLookup(lookup.Find, name); found && strategy.Order != nil {
			return strategy, true
		}
	}
	return Strategy{}, false
}

func safeLookup(find func(string) (Strategy, bool), name string) (strategy Strategy, found bool) {
	defer func() {
		if recover() != nil {
			strategy, found = Strategy{}, false
		}
	}()
	return find(name)
}

func safeList(list func() []DiscoveredRouting) (out []DiscoveredRouting) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	return list()
}

func Discovered() []DiscoveredRouting {
	out := []DiscoveredRouting{}
	seen := map[string]bool{}
	for _, lookup := range slices.Backward(lookupSnapshot()) {
		if lookup.List == nil {
			continue
		}
		for _, entry := range safeList(lookup.List) {
			if !strings.Contains(entry.ID, "/") || seen[entry.ID] {
				continue
			}
			reg.RLock()
			registered := len(reg.m[entry.ID]) != 0
			reg.RUnlock()
			if registered {
				continue
			}
			seen[entry.ID] = true
			out = append(out, entry)
		}
	}
	return out
}
