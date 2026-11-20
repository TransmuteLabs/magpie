package gateway

import "github.com/yetone/magpie/internal/provider"

var routingHarness Server

var (
	weigh        = routingHarness.weigh
	weighed      = routingHarness.weighed
	affine       = routingHarness.affine
	barredOf     = routingHarness.barredOf
	sinks        = routingHarness.sinks
	weightedKept = routingHarness.weightedKept
)

func route(p provider.Provider, cs []candidate, model string, from provider.Protocol) []candidate {
	ordered, _ := routingHarness.weigh(p, cs, model, from)
	return ordered
}
