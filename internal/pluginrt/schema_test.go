package pluginrt

import (
	"reflect"
	"strings"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func testFeatureSettings() []sdk.FeatureSetting {
	minimum, maximum := 0.0, 1.0
	return []sdk.FeatureSetting{
		{Name: "page", Title: "Rows at a time", Kind: sdk.FeatureSettingInteger, Default: 20, Enum: []any{10, 20, 50}},
		{Name: "ratio", Title: "Ratio", Kind: sdk.FeatureSettingNumber, Default: 0.0, Minimum: &minimum, Maximum: &maximum},
		{Name: "shown", Title: "Shown", Kind: sdk.FeatureSettingBoolean, Default: false},
		{Name: "name", Title: "Name", Kind: sdk.FeatureSettingString, Default: ""},
	}
}

// Only what the schema allows is taken, as JSON and YAML give it.
func TestValidate(t *testing.T) {
	s, err := SchemaOf(testFeatureSettings())
	if err != nil {
		t.Fatal(err)
	}
	ok, err := s.Validate(map[string]any{"page": float64(50), "ratio": 1, "shown": true, "name": "y"})
	if err != nil || !reflect.DeepEqual(ok, map[string]any{"page": int64(50), "ratio": 1.0, "shown": true, "name": "y"}) {
		t.Fatalf("good: %v %v", ok, err)
	}
	for _, bad := range []map[string]any{
		{"page": 30.0}, {"page": 20.5}, {"page": "20"}, {"ratio": 2.0}, {"shown": "yes"}, {"name": 1}, {"other": 1},
	} {
		if _, err := s.Validate(bad); err == nil {
			t.Errorf("%v taken", bad)
		}
	}
}

// SetConfig keeps the user's value in plugins.yml, the rest of the file as
// it was; a value the schema refuses writes nothing; Compose leaves out a
// value edited in by hand that it doesn't allow.
func TestSetConfig(t *testing.T) {
	home(t)
	registerTestFeature(t, sdk.FeatureContribution{ID: "schema-test", Settings: testFeatureSettings()})
	m := Registered()[slicesIndex(Registered(), "schema-test")]
	if m.Defaults["page"] != int64(20) || m.Schema() == nil {
		t.Fatalf("registered: %+v", m)
	}
	if err := Set("other", false); err != nil {
		t.Fatal(err)
	}
	if err := SetConfig("schema-test", map[string]any{"page": 50.0}); err != nil {
		t.Fatal(err)
	}
	if err := SetConfig("schema-test", map[string]any{"page": 30.0}); err == nil || !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("30 taken: %v", err)
	}
	if err := SetConfig("nope", map[string]any{"page": 50.0}); err == nil {
		t.Fatal("an unknown plugin taken")
	}
	f, _, err := Load()
	if err != nil || len(f.Plugins) != 2 || f.Plugins[0].ID != "other" || f.Plugins[1].Config["page"] != 50 {
		t.Fatalf("after: %+v %v", f, err)
	}
	c := byID(Compose([]Manifest{m}, f))["schema-test"]
	if c.Config["page"] != int64(50) || c.Sources["page"] != FromUser {
		t.Fatalf("composed: %+v", c)
	}
	f.Plugins[1].Config["page"] = 7
	c = byID(Compose([]Manifest{m}, f))["schema-test"]
	if c.Config["page"] != int64(20) || c.Sources["page"] != FromDefault {
		t.Fatalf("a value it doesn't allow: %+v", c)
	}
}

func slicesIndex(ms []Manifest, id string) int {
	for i, m := range ms {
		if m.ID == id {
			return i
		}
	}
	return -1
}
