package extension

import (
	"encoding/json"
	"fmt"
	"strings"
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

func TestLibraryRuleContribution(test *testing.T) {
	manifest := testManifest(ModuleRPC)
	manifest.Contract.Version.Minor = 17
	manifest.Contributions = []ContributionDescriptor{{
		Point: ContributionLibraryRule, Version: 1,
		LibraryRule: &LibraryRuleDescriptor{ID: "style", Body: "# Style", Paths: []string{"src/**"}},
	}}
	if err := manifest.Validate(); err != nil {
		test.Fatal(err)
	}
	clone := manifest.Clone()
	clone.Contributions[0].LibraryRule.Paths[0] = "changed"
	if manifest.Contributions[0].LibraryRule.Paths[0] != "src/**" || len(manifest.Public().Contributions) != 0 {
		test.Fatal("Library rule escaped installation metadata")
	}
	for name, change := range map[string]func(*Manifest){
		"older API":    func(value *Manifest) { value.Contract.Version.Minor = 16 },
		"duplicate":    func(value *Manifest) { value.Contributions = append(value.Contributions, value.Contributions[0]) },
		"empty":        func(value *Manifest) { value.Contributions[0].LibraryRule.Body = "\u0085" },
		"body size":    func(value *Manifest) { value.Contributions[0].LibraryRule.Body = strings.Repeat("市", 33_334) },
		"glob newline": func(value *Manifest) { value.Contributions[0].LibraryRule.Paths[0] = "bad\npath" },
		"mixed owners": func(value *Manifest) { value.Contributions[0].Search = &SearchSourceDescriptor{ID: "search"} },
	} {
		test.Run(name, func(test *testing.T) {
			invalid := manifest.Clone()
			change(&invalid)
			if err := invalid.Validate(); err == nil {
				test.Fatal("accepted invalid Library rule")
			}
		})
	}
}

func TestLibraryCommandContribution(test *testing.T) {
	manifest := testManifest(ModuleRPC)
	manifest.Contract.Version.Minor = 18
	manifest.Compatibility = &Compatibility{HostAPI: "^1.18"}
	manifest.Contributions = []ContributionDescriptor{{
		Point: ContributionLibraryCommand, Version: 1,
		LibraryCommand: &LibraryCommandDescriptor{
			ID: "review", Description: strings.Repeat("市", MaxLibraryCommandDescription), Prompt: "Review $1",
		},
	}}
	if err := manifest.Validate(); err != nil {
		test.Fatal(err)
	}
	clone := manifest.Clone()
	clone.Contributions[0].LibraryCommand.Prompt = "changed"
	if manifest.Contributions[0].LibraryCommand.Prompt != "Review $1" || len(manifest.Public().Contributions) != 0 {
		test.Fatal("Library command escaped installation metadata")
	}
	for name, change := range map[string]func(*Manifest){
		"older API": func(value *Manifest) {
			value.Contract.Version.Minor = 17
			value.Compatibility = nil
		},
		"duplicate": func(value *Manifest) {
			value.Contributions = append(value.Contributions, value.Contributions[0])
		},
		"too many": func(value *Manifest) {
			for index := range MaxLibraryCommands {
				descriptor := LibraryCommandDescriptor{ID: fmt.Sprintf("command-%d", index), Prompt: "Review"}
				value.Contributions = append(value.Contributions, ContributionDescriptor{
					Point: ContributionLibraryCommand, Version: 1, LibraryCommand: &descriptor,
				})
			}
		},
		"empty prompt": func(value *Manifest) { value.Contributions[0].LibraryCommand.Prompt = "\u0085" },
		"prompt size":  func(value *Manifest) { value.Contributions[0].LibraryCommand.Prompt = strings.Repeat("市", 33_334) },
		"prompt NUL":   func(value *Manifest) { value.Contributions[0].LibraryCommand.Prompt = "bad\x00prompt" },
		"description size": func(value *Manifest) {
			value.Contributions[0].LibraryCommand.Description += "市"
		},
		"description NUL": func(value *Manifest) { value.Contributions[0].LibraryCommand.Description = "bad\x00description" },
		"mixed points":    func(value *Manifest) { value.Contributions[0].LibraryRule = &LibraryRuleDescriptor{ID: "review"} },
	} {
		test.Run(name, func(test *testing.T) {
			invalid := manifest.Clone()
			change(&invalid)
			if err := invalid.Validate(); err == nil {
				test.Fatal("accepted invalid Library command")
			}
		})
	}
}
