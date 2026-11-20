package routes

import (
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var rotation = [...]struct {
	id, name string
	strategy Strategy
	order    func(*Catalog, string, []Seat) []int
}{
	{provider.Ordered, "In order", Ordered, nil},
	{provider.Rotate, "In turn", Strategy{}, (*Catalog).rotate},
	{provider.LeastUsed, "Least used first", LeastUsed, nil},
	{provider.Pace, "Use before expiry", Pace, nil},
	{provider.Weighted, "By weight", Strategy{}, (*Catalog).weighted},
	{provider.FreeFirst, "Free first", FreeFirst, nil},
	{provider.Fastest, "Speed first", Strategy{BySpeed: true}, (*Catalog).fastest},
	{provider.Cheapest, "Cheapest first", Cheapest, nil},
	{provider.Drain, "Until used up", Strategy{Exact: true}, (*Catalog).drain},
	{provider.FreeExpiring, "Free quota by expiry", FreeExpiring, nil},
	{provider.SmallFirst, "Small context first", SmallFirst, nil},
}

func RotationOptions() []Option {
	options := make([]Option, 0, len(rotation))
	for _, definition := range rotation {
		options = append(options, Option{ID: definition.id, Name: definition.name})
	}
	return options
}

func (c *Catalog) RotationStrategies() []sdk.LinkedRoutingRegistration {
	declared := make([]sdk.LinkedRoutingRegistration, 0, len(rotation))
	for _, definition := range rotation {
		strategy := definition.strategy
		if order := definition.order; order != nil {
			strategy.Order = func(pool string, seats []Seat) []int { return order(c, pool, seats) }
		}
		declared = append(declared, sdk.LinkedRoutingRegistration{ID: definition.id, Name: definition.name, Strategy: strategy})
	}
	return declared
}
