package extension

import (
	"encoding/json"
	"testing"
)

func TestDeclarativeMaskingContract(t *testing.T) {
	manifest := testManifest(MaskingRules)
	manifest.Contract.Version.Minor = 15
	manifest.MaskingRuleSets = []MaskingRuleSet{{ID: "team", Rules: []MaskingRule{
		{Kind: "SDK_TOKEN", Prefix: "acme-key-"},
		{Kind: "SDK_TICKET", Regex: `acct-[A-Z0-9]{8,12}`},
	}}}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if methods := RequiredMethods(manifest); len(methods) != 0 {
		t.Fatal(methods)
	}
	for _, invalid := range []MaskingRule{
		{Kind: "SECRET", Prefix: "test-"},
		{Kind: "SDK_0", Prefix: "test-"},
		{Kind: "SDK_TEST", Prefix: "x"},
		{Kind: "SDK_TEST", Regex: `test-[A-Z]{0,8}`},
		{Kind: "SDK_TEST", Regex: `test-[Z-A]{8}`},
		{Kind: "SDK_TEST", Regex: `test-[A-Z]{8,129}`},
		{Kind: "SDK_TEST", Regex: `test-(a+)+`},
	} {
		copy := manifest.Clone()
		copy.MaskingRuleSets[0].Rules[0] = invalid
		if err := copy.Validate(); err == nil {
			t.Fatalf("invalid masking rule admitted: %+v", invalid)
		}
	}
	older := manifest.Clone()
	older.Contract.Version.Minor = 14
	if err := older.Validate(); err == nil {
		t.Fatal("older minor opts in")
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal("clone mutated catalog rules", err)
	}
	var rule MaskingRule
	if json.Unmarshal([]byte(`{"Kind":"SDK_TEST","prefix":"test-"}`), &rule) == nil {
		t.Fatal("accepted noncanonical field")
	}
}
