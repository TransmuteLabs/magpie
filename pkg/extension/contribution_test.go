package extension

import (
	"encoding/json"
	"testing"
)

func TestVersionedOwnerContributions(test *testing.T) {
	manifest := testManifest(ModuleRPC)
	manifest.Contract.Version.Minor = 16
	manifest.Actions = []ActionDescriptor{{ID: "read", Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	manifest.RoutingStrategies = []RoutingStrategyDescriptor{{ID: "previous", Name: "Previous", Action: "read"}}
	manifest.Contributions = []ContributionDescriptor{
		{Point: ContributionRouting, Version: 1, Routing: &RoutingStrategyDescriptor{ID: "route", Name: "Routing", Action: "read"}},
		{Point: ContributionSearch, Version: 1, Search: &SearchSourceDescriptor{ID: "web", Name: "Search", Action: "read"}},
	}
	if err := manifest.Validate(); err != nil {
		test.Fatal(err)
	}
	routing, search := manifest.DeclaredRoutingStrategies(), manifest.DeclaredSearchSources()
	if len(routing) != 2 || routing[0].ID != "previous" || routing[1].ID != "route" || len(search) != 1 || search[0].ID != "web" {
		test.Fatalf("wrong registry projection: %v / %v", routing, search)
	}
	clone := manifest.Clone()
	clone.Contributions[0].Routing.ID = "changed"
	if manifest.Contributions[0].Routing.ID != "route" || len(manifest.Public().Contributions) != 0 {
		test.Fatal("handshake or clone leaked the installation descriptor")
	}
	for name, change := range map[string]func(*Manifest){
		"unnegotiated version": func(value *Manifest) { value.Contract.Version.Minor = 15 },
		"unknown point":        func(value *Manifest) { value.Contributions[0].Point = "unowned" },
		"future contract":      func(value *Manifest) { value.Contributions[0].Version = 2 },
		"wrong owner data":     func(value *Manifest) { value.Contributions[0].Point = ContributionSearch },
		"both owners":          func(value *Manifest) { value.Contributions[0].Search = value.Contributions[1].Search },
		"legacy collision":     func(value *Manifest) { value.Contributions[0].Routing.ID = "previous" },
		"undeclared action":    func(value *Manifest) { value.Contributions[0].Routing.Action = "missing" },
		"write action":         func(value *Manifest) { value.Actions[0].Effect = "write" },
	} {
		test.Run(name, func(test *testing.T) {
			invalid := manifest.Clone()
			change(&invalid)
			if err := invalid.Validate(); err == nil {
				test.Fatal("accepted invalid contribution")
			}
		})
	}
}
