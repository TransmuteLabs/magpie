package pluginrt

import (
	"fmt"

	sdk "github.com/yetone/magpie/pkg/extension"
)

// Schema is a plugin's settings as JSON Schema says an object: the window
// draws a form from it. Order is the fields' order, as JSON keeps no order
// of an object's keys.
type Schema struct {
	Type       string               `json:"type"` // "object"
	Properties map[string]*Property `json:"properties"`
	Order      []string             `json:"x-order"`
}

// Property is one setting.
type Property struct {
	Type        string   `json:"type"` // boolean, integer, number, string
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Enum        []any    `json:"enum,omitempty"`
	Minimum     *float64 `json:"minimum,omitempty"`
	Maximum     *float64 `json:"maximum,omitempty"`
	Default     any      `json:"default"`
}

func (p *Property) value(v any) (any, error) {
	setting := sdk.FeatureSetting{
		Kind: sdk.FeatureSettingKind(p.Type), Enum: p.Enum, Minimum: p.Minimum, Maximum: p.Maximum,
	}
	return setting.Value(v)
}

// Validate gives config as the schema's types, or says the first key or
// value it doesn't allow.
func (s *Schema) Validate(config map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for k, v := range config {
		p := s.Properties[k]
		if p == nil {
			return nil, fmt.Errorf("no setting %q", k)
		}
		x, err := p.value(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = x
	}
	return out, nil
}
