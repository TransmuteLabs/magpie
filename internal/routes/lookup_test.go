package routes

import (
	"slices"
	"testing"
)

func TestLazyLookupNeverOverridesRegisteredOrSmart(test *testing.T) {
	findCalls := 0
	removeLookup := RegisterLookup(Lookup{
		Find: func(name string) (Strategy, bool) {
			findCalls++
			remove := Register("lookup/nested", Strategy{Order: func(string, []Seat) []int { return []int{0, 1} }})
			remove()
			return Strategy{Exact: true, Order: func(string, []Seat) []int { return []int{1, 0} }}, true
		},
		List: func() []DiscoveredRouting {
			return []DiscoveredRouting{{ID: "lookup/reverse", Name: "Reverse", Plugin: "lookup"}, {ID: "smart", Name: "Bad"}}
		},
	})
	defer removeLookup()
	if strategy, found := Find(""); !found || strategy.Order == nil || findCalls != 0 {
		test.Fatal("lookup replaced Smart")
	}
	if _, found := Find("not-a-namespace"); found || findCalls != 0 {
		test.Fatal("lookup served an unqualified built-in name")
	}
	strategy, found := Find("lookup/reverse")
	if !found || !strategy.Exact || !slices.Equal(Apply(strategy, "pool", []Seat{{}, {}}), []int{1, 0}) {
		test.Fatal("qualified lazy strategy was not served")
	}
	unregister := Register("lookup/reverse", Strategy{Order: func(string, []Seat) []int { return []int{0, 1} }})
	defer unregister()
	strategy, found = Find("lookup/reverse")
	if !found || !slices.Equal(Apply(strategy, "pool", []Seat{{}, {}}), []int{0, 1}) || len(Discovered()) != 0 {
		test.Fatal("metadata lookup overrode a registered strategy")
	}
	unregister()
	if metadata := Discovered(); len(metadata) != 1 || metadata[0].ID != "lookup/reverse" {
		test.Fatal("qualified discovery did not recover after registered removal")
	}
	removeLookup()
	if _, found := Find("lookup/reverse"); found {
		test.Fatal("removed lazy lookup still serves")
	}
}

func TestLookupFailureLeavesUnknownAndRegistryUnlocked(test *testing.T) {
	remove := RegisterLookup(Lookup{
		Find: func(string) (Strategy, bool) { panic("private fixture") },
		List: func() []DiscoveredRouting { panic("private fixture") },
	})
	defer remove()
	if _, found := Find("lookup/failed"); found || len(Discovered()) != 0 {
		test.Fatal("panicking lookup changed ordinary routing availability")
	}
	unregister := Register("after-panic", Strategy{Order: func(string, []Seat) []int { return nil }})
	unregister()
}
