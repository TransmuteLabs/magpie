package extension

import (
	"fmt"
	"math"
	"strings"
	"unicode"
)

const (
	MaxRoutingStrategies = 64
	MaxRoutingSeats      = 256
	MaxRoutingRenewals   = 32
	MaxRoutingBytes      = 1 << 20
	RoutingTimeoutMs     = 500
	MaxRoutingTimeoutMs  = 2000
)

type RoutingFlags struct {
	Exact   bool `json:"exact,omitempty"`
	ByUse   bool `json:"byUse,omitempty"`
	ByFree  bool `json:"byFree,omitempty"`
	ByPlan  bool `json:"byPlan,omitempty"`
	BySpeed bool `json:"bySpeed,omitempty"`
	ByPrice bool `json:"byPrice,omitempty"`
}

type RoutingStrategyDescriptor struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Action    string       `json:"action"`
	Flags     RoutingFlags `json:"flags,omitempty"`
	TimeoutMs int          `json:"timeoutMs,omitempty"`
}

type RoutingSeat struct {
	Key       string  `json:"key"`
	Account   bool    `json:"account,omitempty"`
	Known     bool    `json:"known,omitempty"`
	Learns    bool    `json:"learns,omitempty"`
	Used      float64 `json:"used,omitempty"`
	WeekLow   bool    `json:"weekLow,omitempty"`
	Soon      []int64 `json:"soon,omitempty"`
	Unreached int     `json:"unreached,omitempty"`
	Lapse     int64   `json:"lapse,omitempty"`
	Lapsing   float64 `json:"lapsing,omitempty"`
	Pace      float64 `json:"pace,omitempty"`
	Weight    int     `json:"weight"`
	Resting   bool    `json:"resting,omitempty"`
	Spent     bool    `json:"spent,omitempty"`
	Tokens    float64 `json:"tokens,omitempty"`
	Free      bool    `json:"free,omitempty"`
	FreePlan  bool    `json:"freePlan,omitempty"`
	Credits   bool    `json:"credits,omitempty"`
	First     bool    `json:"first,omitempty"`
	Lead      bool    `json:"lead,omitempty"`
	TTFT      float64 `json:"ttft,omitempty"`
	TPS       float64 `json:"tps,omitempty"`
	Left      float64 `json:"left,omitempty"`
	LeftIn    string  `json:"leftIn,omitempty"`
	Priced    bool    `json:"priced,omitempty"`
	Cost      float64 `json:"cost,omitempty"`
	CacheCost float64 `json:"cacheCost,omitempty"`
}

type RoutingRequest struct {
	Strategy string        `json:"strategy"`
	Pool     string        `json:"pool"`
	Seats    []RoutingSeat `json:"seats"`
}

type RoutingResult struct {
	Order []int `json:"order"`
}

func (manifest Manifest) validateRouting() error {
	if len(manifest.RoutingStrategies) == 0 {
		return nil
	}
	if manifest.Contract.Version.Minor < 4 || !manifest.Contract.Has(ModuleRPC) || manifest.Component != nil ||
		len(manifest.RoutingStrategies) > MaxRoutingStrategies {
		return fmt.Errorf("routing strategies require root SDK API 1.4 module-rpc within descriptor limits")
	}
	seen := make(map[string]bool, len(manifest.RoutingStrategies))
	for _, descriptor := range manifest.RoutingStrategies {
		action, found := manifest.ResolveAction(descriptor.Action)
		if !validID(descriptor.ID) || !validName(descriptor.Name) || !validID(descriptor.Action) ||
			seen[descriptor.ID] || !found || action.Effect != "read" ||
			descriptor.TimeoutMs != 0 && (descriptor.TimeoutMs < 25 || descriptor.TimeoutMs > MaxRoutingTimeoutMs) {
			return fmt.Errorf("invalid routing strategy %q or undeclared read action", descriptor.ID)
		}
		seen[descriptor.ID] = true
	}
	return nil
}

func (request RoutingRequest) Validate() error {
	if !validID(request.Strategy) || request.Pool == "" || len(request.Pool) > 512 || strings.ContainsFunc(request.Pool, unicode.IsControl) ||
		len(request.Seats) == 0 || len(request.Seats) > MaxRoutingSeats {
		return fmt.Errorf("invalid routing request bounds")
	}
	for _, seat := range request.Seats {
		if !validID(seat.Key) || len(seat.Soon) > MaxRoutingRenewals || seat.Weight < 1 || seat.Weight > 1_000_000 ||
			seat.Unreached < 0 || seat.Unreached > len(seat.Soon) ||
			seat.LeftIn != "" && !validID(seat.LeftIn) || seat.Lapse < -9_007_199_254_740_991 || seat.Lapse > 9_007_199_254_740_991 {
			return fmt.Errorf("invalid routing seat bounds")
		}
		for _, number := range []float64{seat.Used, seat.Lapsing, seat.Pace, seat.Tokens, seat.TTFT, seat.TPS, seat.Left, seat.Cost, seat.CacheCost} {
			if math.IsNaN(number) || math.IsInf(number, 0) || math.Abs(number) > 1e15 {
				return fmt.Errorf("routing measurements must be finite and bounded")
			}
		}
		for _, instant := range seat.Soon {
			if instant < -9_007_199_254_740_991 || instant > 9_007_199_254_740_991 {
				return fmt.Errorf("routing renewal exceeds safe timestamp bounds")
			}
		}
	}
	return nil
}

func (result RoutingResult) Validate(seats int) error {
	if seats < 1 || seats > MaxRoutingSeats || len(result.Order) != seats {
		return fmt.Errorf("routing order must include every seat")
	}
	seen := make([]bool, seats)
	for _, index := range result.Order {
		if index < 0 || index >= seats || seen[index] {
			return fmt.Errorf("routing order must be an index permutation")
		}
		seen[index] = true
	}
	return nil
}
