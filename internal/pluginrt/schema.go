package pluginrt

import (
	"fmt"
	"math"
	"slices"
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
	var out any
	switch p.Type {
	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%v is not true or false", v)
		}
		out = b
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%v is not text", v)
		}
		out = s
	case "integer", "number":
		var f float64
		switch n := v.(type) {
		case int:
			f = float64(n)
		case int64:
			f = float64(n)
		case float64:
			f = n
		default:
			return nil, fmt.Errorf("%v is not a number", v)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("%v is not a number", v)
		}
		if p.Minimum != nil && f < *p.Minimum || p.Maximum != nil && f > *p.Maximum {
			return nil, fmt.Errorf("%v is out of range", v)
		}
		if p.Type == "integer" {
			if f != math.Trunc(f) {
				return nil, fmt.Errorf("%v is not a whole number", v)
			}
			if math.Abs(f) > 1<<53-1 {
				return nil, fmt.Errorf("%v is outside the exact JSON integer range", v)
			}
			out = int64(f)
		} else {
			out = f
		}
	}
	if len(p.Enum) > 0 && !slices.Contains(p.Enum, out) {
		return nil, fmt.Errorf("%v is not one of %v", v, p.Enum)
	}
	return out, nil
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
