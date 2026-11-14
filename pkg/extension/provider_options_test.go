package extension

import (
	"encoding/json"
	"strconv"
	"testing"
)

func providerOptionsFixture(test *testing.T, minor int, options string) Manifest {
	test.Helper()
	var manifest Manifest
	body := []byte(`{"schema":1,"id":"fixture.options","version":"1.0.0","runtime":"exec","entry":"worker",
		"contract":{"version":{"major":1,"minor":` + strconv.Itoa(minor) + `},"capabilities":["outbound-executor"]},
		"providers":[{"id":"vendor","name":"Vendor","protocols":["chat"],"options":` + options + `}]}`)
	if err := json.Unmarshal(body, &manifest); err != nil {
		test.Fatal(err)
	}
	return manifest
}

func TestProviderOptionsRetainTheirDeclarationAndClone(test *testing.T) {
	manifest := providerOptionsFixture(test, 9, `[{"key":"paid","label":"Paid <mode>","default":false,
		"confirm":"Uses account credits","labels":{"de":"Bezahlt"},"placement":"top","filtersModels":true}]`)
	if err := manifest.Validate(); err != nil {
		test.Fatal(err)
	}
	snapshot := manifest.Clone()
	raw, err := json.Marshal(snapshot.Public())
	if err != nil {
		test.Fatal(err)
	}
	var fields struct {
		Providers []struct {
			Options []map[string]any `json:"options"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		test.Fatal(err)
	}
	if len(fields.Providers) != 1 || len(fields.Providers[0].Options) != 1 ||
		fields.Providers[0].Options[0]["key"] != "paid" || fields.Providers[0].Options[0]["confirm"] != "Uses account credits" {
		test.Fatalf("public option metadata was discarded: %s", raw)
	}
	snapshot.Providers[0].Options[0].Labels["de"] = "Changed"
	if manifest.Providers[0].Options[0].Labels["de"] != "Bezahlt" {
		test.Fatal("a copied provider switch changed the installed declaration")
	}
}

func TestProviderOptionsRejectUndeclaredContractAndMalformedSwitches(test *testing.T) {
	for _, scenario := range []struct {
		name    string
		minor   int
		options string
	}{
		{"old contract", 8, `[{"key":"paid","label":"Paid"}]`},
		{"empty key", 9, `[{"key":"","label":"Paid"}]`},
		{"prototype key", 9, `[{"key":"constructor","label":"Paid"}]`},
		{"duplicate key", 9, `[{"key":"paid","label":"Paid"},{"key":"paid","label":"Other"}]`},
		{"empty label", 9, `[{"key":"paid","label":""}]`},
		{"unknown placement", 9, `[{"key":"paid","label":"Paid","placement":"outside"}]`},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			if err := providerOptionsFixture(test, scenario.minor, scenario.options).Validate(); err == nil {
				test.Fatal("malformed or unnegotiated provider switch was accepted")
			}
		})
	}
}

func TestProviderOptionsValidateOnlyDeclaredConfigurationFields(test *testing.T) {
	manifest := providerOptionsFixture(test, 9, `[{"key":"paid","label":"Paid","default":true}]`)
	owner, ok := any(manifest).(interface {
		ValidateProviderOptionValues(json.RawMessage) error
	})
	if !ok {
		test.Fatal("provider option configuration has no checked owner")
	}
	for _, values := range []string{`{}`, `{"paid":false}`, `{"paid":true,"unrelated":{"keep":1}}`} {
		if err := owner.ValidateProviderOptionValues(json.RawMessage(values)); err != nil {
			test.Fatalf("valid configuration was rejected: %s: %v", values, err)
		}
	}
	for _, values := range []string{`{"paid":null}`, `{"paid":"true"}`, `{"paid":1}`, `[]`, `null`} {
		if err := owner.ValidateProviderOptionValues(json.RawMessage(values)); err == nil {
			test.Fatalf("invalid option configuration was accepted: %s", values)
		}
	}
}
