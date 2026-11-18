package extension

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"

	"github.com/yetone/magpie/pkg/protocolext"
)

func accountBindingManifest() Manifest {
	return Manifest{
		Schema: 1, ID: "account-window", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract:  Contract{Version: APIVersion{Major: 1, Minor: 5}, Capabilities: []Capability{ModuleRPC, AccountAuth, ModelDiscovery}},
		Providers: []ProviderDescriptor{{ID: "vendor", Name: "Vendor", Protocols: []protocolext.Protocol{protocolext.Chat}}},
		Actions:   []ActionDescriptor{{ID: "inspect", Effect: "read", InputSchema: json.RawMessage(`{}`)}},
		Window:    &WindowDescriptor{Metadata: "package.json"},
		Bindings:  []BindingDescriptor{{ID: "page.inspect", From: "window:page", Action: "inspect", Provider: "vendor"}},
	}
}

func TestWindowBindingAccountGrantRequiresAPI15(test *testing.T) {
	source := accountBindingManifest()
	if err := source.Validate(); err != nil {
		test.Fatal(err)
	}
	for _, mutate := range []func(*Manifest){
		func(value *Manifest) { value.Contract.Version.Minor = 4 },
		func(value *Manifest) { value.Contract.Capabilities = []Capability{ModuleRPC, ModelDiscovery} },
		func(value *Manifest) { value.Bindings[0].Provider = "foreign" },
		func(value *Manifest) { value.Bindings[0].From = PrimaryComponent },
		func(value *Manifest) { value.Bindings[0].Action = "" },
	} {
		value := source.Clone()
		mutate(&value)
		if err := value.Validate(); err == nil {
			test.Fatal("invalid selected-account binding grant was accepted")
		}
	}
	legacy := source.Clone()
	legacy.Contract.Version.Minor = 4
	legacy.Bindings[0].Provider = ""
	if err := legacy.Validate(); err != nil {
		test.Fatal("legacy root binding changed:", err)
	}
}

func TestAccountSelectionPreservesFullGenerationAndRejectsAuthority(test *testing.T) {
	for _, generation := range []uint64{1, 9007199254740993, math.MaxUint64} {
		expected := strconv.FormatUint(generation, 10)
		var selection AccountSelection
		raw := []byte(`{"id":"user@example.invalid","expectedGeneration":"` + expected + `"}`)
		if err := json.Unmarshal(raw, &selection); err != nil {
			test.Fatal(err)
		}
		actual, err := selection.Validate()
		if err != nil || actual != generation {
			test.Fatal("account generation was changed:", actual, err)
		}
	}
	for _, raw := range []string{
		`null`, `{}`, `{"id":"one","expectedGeneration":"0"}`, `{"id":"one","expectedGeneration":"01"}`,
		`{"id":"one","expectedGeneration":1}`, `{"id":"one","expectedGeneration":"18446744073709551616"}`,
		`{"id":"one","expectedGeneration":"1","provider":"other"}`, `{"id":"one","expectedGeneration":"1","auth":{}}`,
	} {
		var selection AccountSelection
		if json.Unmarshal([]byte(raw), &selection) == nil {
			test.Fatalf("invalid account selection accepted: %s", raw)
		}
	}
}
