package extensionrouting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/routes"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func Source(manager *extension.Manager) routes.Source {
	return routes.Source{
		Find: func(name string) (routes.Strategy, bool) {
			binding, found := manager.RoutingStrategy(name)
			if !found {
				return routes.Strategy{}, false
			}
			return Strategy(manager, binding), true
		},
		List: func() []routes.Option {
			bindings := manager.RoutingStrategies()
			out := make([]routes.Option, len(bindings))
			for index, binding := range bindings {
				out[index] = routes.Option{
					ID:   binding.Module + "/" + binding.Descriptor.ID,
					Name: binding.Descriptor.Name, Plugin: binding.Module,
				}
			}
			return out
		},
	}
}

func Strategy(manager *extension.Manager, binding extension.RoutingBinding) routes.Strategy {
	flags := binding.Descriptor.Flags
	return routes.Strategy{
		Exact: flags.Exact, ByUse: flags.ByUse, ByFree: flags.ByFree, ByPlan: flags.ByPlan, BySpeed: flags.BySpeed, ByPrice: flags.ByPrice,
		Order: func(pool string, seats []routes.Seat) []int {
			identity := make([]int, len(seats))
			for index := range identity {
				identity[index] = index
			}
			if len(seats) < 2 {
				return identity
			}
			request, err := routingRequest(binding.Module, binding.Descriptor, pool, seats)
			if err == nil {
				result, callErr := manager.OrderRouting(context.Background(), binding, request)
				if callErr == nil {
					return result.Order
				}
			}
			log.Printf("extension routing: %s/%s failed; trying listed order", binding.Module, binding.Descriptor.ID)
			return identity
		},
	}
}

func routingOpaque(module, pool, key string) string {
	digest := sha256.Sum256([]byte(module + "\x00" + pool + "\x00" + key))
	return hex.EncodeToString(digest[:])
}

func routingTimestamp(instant time.Time) int64 {
	if instant.IsZero() {
		return 0
	}
	return instant.UnixMilli()
}

func routingRequest(module string, descriptor sdk.RoutingStrategyDescriptor, pool string, seats []routes.Seat) (sdk.RoutingRequest, error) {
	if len(seats) == 0 || len(seats) > sdk.MaxRoutingSeats {
		return sdk.RoutingRequest{}, errors.New("routing pool exceeds its seat budget")
	}
	request := sdk.RoutingRequest{Strategy: descriptor.ID, Pool: pool, Seats: make([]sdk.RoutingSeat, len(seats))}
	for index, seat := range seats {
		if len(seat.Soon) > sdk.MaxRoutingRenewals {
			return sdk.RoutingRequest{}, errors.New("routing renewals exceed their budget")
		}
		soon := make([]int64, len(seat.Soon))
		for renewal, instant := range seat.Soon {
			soon[renewal] = routingTimestamp(instant)
		}
		wallet := ""
		if seat.LeftIn != "" {
			wallet = routingOpaque(module, "wallet", seat.LeftIn)
		}
		request.Seats[index] = sdk.RoutingSeat{
			Key: routingOpaque(module, pool, seat.Key), Account: seat.Account, Known: seat.Known, Learns: seat.Learns,
			Used: seat.Used, WeekLow: seat.WeekLow, Soon: soon, Unreached: seat.Unreached, Lapse: routingTimestamp(seat.Lapse),
			Lapsing: seat.Lapsing, Pace: seat.Pace, Weight: seat.Weight, Resting: seat.Resting, Spent: seat.Spent,
			Tokens: seat.Tokens, Free: seat.Free, FreePlan: seat.FreePlan, Credits: seat.Credits, First: seat.First,
			Lead: seat.Lead, TTFT: seat.TTFT, TPS: seat.TPS, Left: seat.Left, LeftIn: wallet, Priced: seat.Priced,
			Cost: seat.Cost, CacheCost: seat.CacheCost,
		}
	}
	return request, request.Validate()
}
