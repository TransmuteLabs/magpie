package routes

import (
	"slices"
	"testing"
)

func TestLazyLookupNeverOverridesRegisteredOrSmart(test *testing.T) {
	findCalls := 0
	base := NewCatalog()
	catalog := base.WithSource(Source{
		Find: func(name string) (Strategy, bool) {
			findCalls++
			remove := base.Register(Option{ID: "lookup/nested"}, Ordered)
			remove()
			return Strategy{Exact: true, Order: func(string, []Seat) []int { return []int{1, 0} }}, true
		},
		List: func() []Option {
			remove := base.Register(Option{ID: "lookup/list"}, Ordered)
			remove()
			return []Option{{ID: "lookup/reverse", Name: "Reverse", Plugin: "lookup"}, {ID: "smart", Name: "Bad"}}
		},
	})
	if strategy, found := catalog.Find(""); !found || strategy.Order == nil || findCalls != 0 {
		test.Fatal("lookup replaced Smart")
	}
	if _, found := catalog.Find("not-a-namespace"); found || findCalls != 0 {
		test.Fatal("lookup served an unqualified built-in name")
	}
	strategy, found := catalog.Find("lookup/reverse")
	if !found || !strategy.Exact || !slices.Equal(Apply(strategy, "pool", []Seat{{}, {}}), []int{1, 0}) {
		test.Fatal("qualified lazy strategy was not served")
	}
	option := Option{ID: "lookup/reverse", Name: "Linked"}
	unregister := base.Register(option, Ordered)
	defer unregister()
	strategy, found = catalog.Find("lookup/reverse")
	if !found || !slices.Equal(Apply(strategy, "pool", []Seat{{}, {}}), []int{0, 1}) ||
		!slices.Equal(catalog.List(), []Option{{Name: "Quota first"}, option}) {
		test.Fatal("metadata lookup overrode a registered strategy")
	}
	unregister()
	if metadata := catalog.List(); len(metadata) != 2 || metadata[1].ID != option.ID || metadata[1].Plugin != "lookup" {
		test.Fatal("qualified discovery did not recover after registered removal")
	}
	if base.Served(option.ID) || len(base.List()) != 1 {
		test.Fatal("binding an installed source changed the original catalog")
	}
}

func TestLookupFailureLeavesUnknownAndRegistryUnlocked(test *testing.T) {
	catalog := NewCatalog().WithSource(Source{
		Find: func(string) (Strategy, bool) { panic("private fixture") },
		List: func() []Option { panic("private fixture") },
	})
	if catalog.Served("lookup/failed") || len(catalog.List()) != 1 {
		test.Fatal("panicking lookup changed ordinary routing availability")
	}
	unregister := catalog.Register(Option{ID: "after-panic"}, Ordered)
	unregister()
}
