package extension

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
)

const ModuleSettingsAPIMinor = 16

func (setting *FeatureSetting) UnmarshalJSON(raw []byte) error {
	type fields FeatureSetting
	var next fields
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&next) != nil || decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid extension setting declaration")
	}
	var properties map[string]json.RawMessage
	if json.Unmarshal(raw, &properties) != nil || properties == nil {
		return fmt.Errorf("extension setting must be an object")
	}
	for key, value := range properties {
		switch key {
		case "name", "kind", "title", "description", "default", "enum", "minimum", "maximum":
		default:
			return fmt.Errorf("unknown extension setting property %q", key)
		}
		if bytes.Equal(value, []byte("null")) {
			return fmt.Errorf("extension setting %q cannot be null", key)
		}
	}
	*setting = FeatureSetting(next)
	return nil
}

func cloneFeatureSettings(settings []FeatureSetting) []FeatureSetting {
	settings = slices.Clone(settings)
	for index := range settings {
		settings[index].Enum = slices.Clone(settings[index].Enum)
		if settings[index].Minimum != nil {
			settings[index].Minimum = new(*settings[index].Minimum)
		}
		if settings[index].Maximum != nil {
			settings[index].Maximum = new(*settings[index].Maximum)
		}
	}
	return settings
}

func (manifest Manifest) validateModuleSettings() error {
	if len(manifest.Settings) == 0 {
		return nil
	}
	if manifest.Contract.Version.Minor < ModuleSettingsAPIMinor || manifest.Component != nil || len(manifest.Settings) > 32 {
		return fmt.Errorf("module settings require a root SDK API 1.16 module and at most 32 settings")
	}
	declared := make(map[string]bool, len(manifest.Settings))
	for _, provider := range manifest.Providers {
		for _, option := range provider.Options {
			declared[option.Key] = true
		}
	}
	for _, setting := range manifest.Settings {
		if !providerOptionKey(setting.Name) || declared[setting.Name] ||
			!providerOptionText(setting.Title, 512) || !providerOptionText(setting.Description, 4096) ||
			len(setting.Enum) > 32 {
			return fmt.Errorf("invalid or duplicate module setting %q", setting.Name)
		}
		declared[setting.Name] = true
		normalized, err := setting.Normalize()
		if err != nil {
			return fmt.Errorf("module setting %q: %w", setting.Name, err)
		}
		seen := make(map[any]bool, len(normalized.Enum))
		for _, value := range normalized.Enum {
			if seen[value] {
				return fmt.Errorf("module setting %q has a duplicate enum value", setting.Name)
			}
			seen[value] = true
		}
		for _, value := range append(slices.Clone(normalized.Enum), normalized.Default) {
			if value, text := value.(string); text && !providerOptionText(value, 4096) {
				return fmt.Errorf("module setting %q has an invalid text value", setting.Name)
			}
		}
	}
	return nil
}

// Normalize validates a scalar declaration and owns the projected bounds and choices.
func (setting FeatureSetting) Normalize() (FeatureSetting, error) {
	if setting.Name == "" || strings.TrimSpace(setting.Name) != setting.Name || strings.TrimSpace(setting.Title) == "" {
		return FeatureSetting{}, fmt.Errorf("invalid name or title")
	}
	switch setting.Kind {
	case FeatureSettingBoolean, FeatureSettingString:
		if setting.Minimum != nil || setting.Maximum != nil {
			return FeatureSetting{}, fmt.Errorf("bounds require a numeric setting")
		}
	case FeatureSettingInteger, FeatureSettingNumber:
	default:
		return FeatureSetting{}, fmt.Errorf("unsupported kind %q", setting.Kind)
	}
	normalized := setting
	if setting.Minimum != nil {
		minimum := *setting.Minimum
		if math.IsNaN(minimum) || math.IsInf(minimum, 0) {
			return FeatureSetting{}, fmt.Errorf("minimum must be finite")
		}
		normalized.Minimum = &minimum
	}
	if setting.Maximum != nil {
		maximum := *setting.Maximum
		if math.IsNaN(maximum) || math.IsInf(maximum, 0) || normalized.Minimum != nil && maximum < *normalized.Minimum {
			return FeatureSetting{}, fmt.Errorf("invalid maximum")
		}
		normalized.Maximum = &maximum
	}
	normalized.Enum = nil
	var choices []any
	for _, value := range setting.Enum {
		choice, err := normalized.Value(value)
		if err != nil {
			return FeatureSetting{}, fmt.Errorf("enum: %w", err)
		}
		choices = append(choices, choice)
	}
	normalized.Enum = choices
	var err error
	normalized.Default, err = normalized.Value(setting.Default)
	if err != nil {
		return FeatureSetting{}, fmt.Errorf("default: %w", err)
	}
	return normalized, nil
}

// Value checks a value against a normalized scalar declaration.
func (setting FeatureSetting) Value(value any) (any, error) {
	var normalized any
	switch setting.Kind {
	case FeatureSettingBoolean:
		if boolean, ok := value.(bool); ok {
			normalized = boolean
		} else {
			return nil, fmt.Errorf("must be true or false")
		}
	case FeatureSettingString:
		if text, ok := value.(string); ok {
			normalized = text
		} else {
			return nil, fmt.Errorf("must be text")
		}
	case FeatureSettingInteger, FeatureSettingNumber:
		var number float64
		switch value := value.(type) {
		case int:
			number = float64(value)
		case int64:
			number = float64(value)
		case float64:
			number = value
		default:
			return nil, fmt.Errorf("must be a number")
		}
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, fmt.Errorf("must be a finite number")
		}
		if setting.Minimum != nil && number < *setting.Minimum || setting.Maximum != nil && number > *setting.Maximum {
			return nil, fmt.Errorf("is out of range")
		}
		if setting.Kind == FeatureSettingInteger {
			if number != math.Trunc(number) {
				return nil, fmt.Errorf("must be a whole number")
			}
			if math.Abs(number) > 1<<53-1 {
				return nil, fmt.Errorf("is outside the exact JSON integer range")
			}
			normalized = int64(number)
		} else {
			normalized = number
		}
	default:
		return nil, fmt.Errorf("unsupported kind %q", setting.Kind)
	}
	if len(setting.Enum) > 0 && !slices.Contains(setting.Enum, normalized) {
		return nil, fmt.Errorf("is not one of the declared choices")
	}
	return normalized, nil
}

func (manifest Manifest) ValidateModuleConfig(values json.RawMessage) error {
	_, err := manifest.moduleSettingValues(values, false)
	if err != nil {
		return err
	}
	return manifest.ValidateProviderOptionValues(values)
}

// EffectiveModuleConfig supplies defaults to the runtime without saving them.
func (manifest Manifest) EffectiveModuleConfig(values json.RawMessage) (json.RawMessage, error) {
	if err := manifest.ValidateProviderOptionValues(values); err != nil {
		return nil, err
	}
	return manifest.moduleSettingValues(values, true)
}

func (manifest Manifest) moduleSettingValues(values json.RawMessage, defaults bool) (json.RawMessage, error) {
	if len(manifest.Settings) == 0 {
		return values, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(values, &fields) != nil || fields == nil {
		return nil, fmt.Errorf("module settings require a JSON object")
	}
	added := false
	for _, declaration := range manifest.Settings {
		setting, err := declaration.Normalize()
		if err != nil {
			return nil, fmt.Errorf("module setting %q: %w", declaration.Name, err)
		}
		raw, present := fields[setting.Name]
		if !present {
			if defaults {
				fields[setting.Name], err = json.Marshal(setting.Default)
				if err != nil {
					return nil, err
				}
				added = true
			}
			continue
		}
		var value any
		if json.Unmarshal(raw, &value) != nil {
			return nil, fmt.Errorf("module setting %q is unreadable", setting.Name)
		}
		if text, ok := value.(string); ok && !providerOptionText(text, 4096) {
			return nil, fmt.Errorf("module setting %q has an invalid text value", setting.Name)
		}
		if _, err := setting.Value(value); err != nil {
			return nil, fmt.Errorf("module setting %q: %w", setting.Name, err)
		}
	}
	if added {
		return json.Marshal(fields)
	}
	return values, nil
}
