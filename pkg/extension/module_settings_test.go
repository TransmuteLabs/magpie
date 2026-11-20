package extension

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/yetone/magpie/pkg/protocolext"
)

func TestModuleSettingsContract(t *testing.T) {
	manifest := Manifest{
		Schema: ManifestSchema, ID: "settings-fixture", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract: Contract{Version: APIVersion{Major: 1, Minor: ModuleSettingsAPIMinor}, Capabilities: []Capability{OutboundExecutor}},
		Providers: []ProviderDescriptor{{ID: "example", Name: "Example", Protocols: []protocolext.Protocol{"chat"},
			Options: []ProviderOption{{Key: "enabled", Label: "Enabled"}}}},
		Settings: []FeatureSetting{
			{Name: "count", Kind: FeatureSettingInteger, Title: "Count", Minimum: new(float64(0)), Maximum: new(float64(10)), Enum: []any{1, 10}, Default: 1},
			{Name: "mode", Kind: FeatureSettingString, Title: "Mode", Default: "normal"},
		},
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	public := manifest.Public()
	if len(public.Settings) != 0 || len(public.Providers) != 1 || len(manifest.Settings) != 2 {
		t.Fatalf("host declarations escaped public projection: %+v", public)
	}
	clone := manifest.Clone()
	*clone.Settings[0].Minimum = 8
	clone.Settings[0].Enum[0] = 8
	if *manifest.Settings[0].Minimum != 0 || manifest.Settings[0].Enum[0] != 1 {
		t.Fatal("manifest clone shares setting bounds or choices")
	}
	for _, scenario := range []struct {
		name  string
		alter func(*Manifest)
	}{
		{"older API", func(value *Manifest) { value.Contract.Version.Minor-- }},
		{"provider switch key", func(value *Manifest) { value.Settings[0].Name = "enabled" }},
		{"duplicate key", func(value *Manifest) { value.Settings[1].Name = "count" }},
		{"duplicate choice", func(value *Manifest) { value.Settings[0].Enum[1] = float64(1) }},
		{"invalid default", func(value *Manifest) { value.Settings[0].Default = 9 }},
		{"non-finite bound", func(value *Manifest) { *value.Settings[0].Maximum = math.Inf(1) }},
		{"oversized title", func(value *Manifest) { value.Settings[0].Title = strings.Repeat("я", 257) }},
		{"oversized text", func(value *Manifest) { value.Settings[1].Default = strings.Repeat("я", 2049) }},
		{"unsafe integer", func(value *Manifest) {
			value.Settings[0].Minimum, value.Settings[0].Maximum, value.Settings[0].Enum = nil, nil, nil
			value.Settings[0].Default = int64(1 << 53)
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			candidate := manifest.Clone()
			scenario.alter(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("invalid settings were accepted")
			}
		})
	}

	for _, raw := range []string{
		`{"count":null}`, `{"count":2}`, `{"count":1.5}`, `{"enabled":"true"}`, `{"mode":"plain\u0000text"}`,
	} {
		if err := manifest.ValidateModuleConfig(json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid settings config was accepted: %s", raw)
		}
	}
	const raw = `{"enabled":true,"private":{"preserve":3}}`
	effective, err := manifest.EffectiveModuleConfig(json.RawMessage(raw))
	if err != nil || string(effective) != `{"count":1,"enabled":true,"mode":"normal","private":{"preserve":3}}` {
		t.Fatalf("effective settings did not preserve unrelated configuration: %s %v", effective, err)
	}
	for _, raw := range []string{`null`, `{"Name":"solid"}`, `{"name":"solid","minimum":null}`, `{"name":"solid","unknown":true}`} {
		var setting FeatureSetting
		if err := json.Unmarshal([]byte(raw), &setting); err == nil {
			t.Fatalf("malformed setting declaration was accepted: %s", raw)
		}
	}
}
