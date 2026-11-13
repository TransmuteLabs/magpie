package extension

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func routingManifest() Manifest {
	return Manifest{
		Schema: ManifestSchema, ID: "routing.fixture", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract:          Contract{Version: APIVersion{Major: 1, Minor: 4}, Capabilities: []Capability{ModuleRPC}},
		Actions:           []ActionDescriptor{{ID: "rank", Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		RoutingStrategies: []RoutingStrategyDescriptor{{ID: "reverse", Name: "Reverse", Action: "rank", Flags: RoutingFlags{Exact: true, ByUse: true, ByFree: true, ByPlan: true, BySpeed: true, ByPrice: true}}},
	}
}

func TestRoutingStrategyDescriptorOwnsOnlyReadAction(test *testing.T) {
	source := routingManifest()
	if err := source.Validate(); err != nil {
		test.Fatal(err)
	}
	for _, change := range []func(*Manifest){
		func(value *Manifest) { value.Contract.Version.Minor = 3 },
		func(value *Manifest) { value.Contract.Capabilities = []Capability{ToolProvider} },
		func(value *Manifest) { value.RoutingStrategies[0].ID = "other/strategy" },
		func(value *Manifest) { value.RoutingStrategies[0].Name = "\n" },
		func(value *Manifest) { value.RoutingStrategies[0].Action = "missing" },
		func(value *Manifest) { value.Actions[0].Effect = "write" },
		func(value *Manifest) { value.RoutingStrategies[0].TimeoutMs = 24 },
		func(value *Manifest) { value.RoutingStrategies[0].TimeoutMs = MaxRoutingTimeoutMs + 1 },
		func(value *Manifest) {
			value.RoutingStrategies = append(value.RoutingStrategies, value.RoutingStrategies[0])
		},
		func(value *Manifest) {
			value.RoutingStrategies = make([]RoutingStrategyDescriptor, MaxRoutingStrategies+1)
		},
	} {
		value := source.Clone()
		change(&value)
		if err := value.Validate(); err == nil {
			test.Fatal("invalid routing authority was accepted")
		}
	}
	snapshot := source.Public()
	source.RoutingStrategies[0].Name = "Changed"
	source.RoutingStrategies[0].Flags.Exact = false
	if snapshot.RoutingStrategies[0].Name != "Reverse" || !snapshot.RoutingStrategies[0].Flags.Exact {
		test.Fatal("public routing metadata retained mutable root storage")
	}
}

func TestRoutingDTOBoundsAndPermutation(test *testing.T) {
	valid := RoutingRequest{Strategy: "reverse", Pool: "group/example",
		Seats: []RoutingSeat{{Key: "opaque-a", Weight: 1, Soon: []int64{123}, Unreached: 1}, {Key: "opaque-b", Weight: 2}}}
	if err := valid.Validate(); err != nil {
		test.Fatal(err)
	}
	for _, change := range []func(*RoutingRequest){
		func(value *RoutingRequest) { value.Seats = nil },
		func(value *RoutingRequest) { value.Seats = make([]RoutingSeat, MaxRoutingSeats+1) },
		func(value *RoutingRequest) { value.Pool = strings.Repeat("x", 513) },
		func(value *RoutingRequest) { value.Pool = "pool\n" },
		func(value *RoutingRequest) { value.Seats[0].Key = "private@example.invalid" },
		func(value *RoutingRequest) { value.Seats[0].Weight = 0 },
		func(value *RoutingRequest) { value.Seats[0].Cost = math.NaN() },
		func(value *RoutingRequest) { value.Seats[0].Pace = math.Inf(1) },
		func(value *RoutingRequest) { value.Seats[0].Tokens = 1e16 },
		func(value *RoutingRequest) { value.Seats[0].Soon = make([]int64, MaxRoutingRenewals+1) },
		func(value *RoutingRequest) { value.Seats[0].Unreached = 2 },
		func(value *RoutingRequest) { value.Seats[0].LeftIn = "provider\x00private" },
	} {
		value := valid
		value.Seats = append([]RoutingSeat(nil), valid.Seats...)
		change(&value)
		if err := value.Validate(); err == nil {
			test.Fatal("unbounded routing request was accepted")
		}
	}
	for _, order := range [][]int{nil, {0}, {0, 0}, {-1, 1}, {0, 2}, {0, 1, 2}} {
		if err := (RoutingResult{Order: order}).Validate(2); err == nil {
			test.Fatalf("invalid routing permutation %v accepted", order)
		}
	}
	if err := (RoutingResult{Order: []int{1, 0}}).Validate(2); err != nil {
		test.Fatal(err)
	}
}

func TestRoutingPublicProjectionKeepsOnlyOwnedActions(test *testing.T) {
	root := routingManifest()
	root.Actions = append(root.Actions, ActionDescriptor{ID: "backend.rank", Component: "backend",
		Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`)})
	root.Services = []ServiceDescriptor{{
		ID: "backend", Runtime: "exec", Entry: "worker", Contract: ServiceContract{ID: "routing.backend", Version: 1},
		Methods: []string{"backend.rank"}, Start: "lazy",
	}}
	root.RoutingStrategies = append(root.RoutingStrategies,
		RoutingStrategyDescriptor{ID: "private", Name: "Private backend", Action: "backend.rank"})
	if err := root.Validate(); err != nil {
		test.Fatal("full root routing declaration rejected:", err)
	}
	public := root.Public()
	public.Runtime, public.Entry = root.Runtime, root.Entry
	if err := public.Validate(); err != nil {
		test.Fatal("primary routing handshake projection is invalid:", err)
	}
	if len(public.RoutingStrategies) != 1 || public.RoutingStrategies[0].ID != "reverse" {
		test.Fatal("primary handshake did not retain exactly its owned routing")
	}
	if len(root.RoutingStrategies) != 2 || root.RoutingStrategies[1].Action != "backend.rank" || len(root.Actions) != 2 {
		test.Fatal("public projection removed the root's private routing authority")
	}
	service, err := ServiceManifest(root, "backend")
	if err != nil || service.Validate() != nil || len(service.RoutingStrategies) != 0 {
		test.Fatal("private routing service manifest is invalid:", err)
	}
	public.RoutingStrategies[0].Name = "mutated"
	if root.RoutingStrategies[0].Name != "Reverse" {
		test.Fatal("routing projection shares the installed root's descriptor storage")
	}
}
