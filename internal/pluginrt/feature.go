package pluginrt

import (
	"fmt"
	"math"
	"slices"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

// RegisterFeature registers a linked SDK contribution under the existing
// built-in manifest's identity and lifecycle.
func RegisterFeature(contribution sdk.FeatureContribution) {
	register(featureManifest(contribution))
}

func featureManifest(contribution sdk.FeatureContribution) Manifest {
	m := Manifest{
		ID: contribution.ID, API: contribution.API,
		Requires: slices.Clone(contribution.Requires), Essential: contribution.Essential,
		Name: contribution.Name, Sub: contribution.Sub, One: contribution.One, Many: contribution.Many,
		InUse: contribution.InUse, Client: slices.Clone(contribution.Client),
	}
	if activate := contribution.Activate; activate != nil {
		m.Apply = func(ctx *Ctx) error { return activate(ctx) }
	}
	if len(contribution.Parts) != 0 {
		parts, err := featureParts(contribution.Parts)
		if err != nil {
			panic(fmt.Errorf("pluginrt: %s UI: %w", m.ID, err))
		}
		m.Parts = parts
	}
	if len(contribution.Settings) != 0 {
		schema, err := SchemaOf(contribution.Settings)
		if err != nil {
			panic(fmt.Errorf("pluginrt: %s settings: %w", m.ID, err))
		}
		m.schema = schema
	}
	if destination := contribution.SettingsAt; destination != nil {
		if !validFeatureDestinationID(destination.Tab) || !validFeatureDestinationID(destination.RowID) {
			panic(fmt.Sprintf("pluginrt: %s invalid settings location %q %q", m.ID, destination.Tab, destination.RowID))
		}
		m.SetAt = destination.Tab + " #" + destination.RowID
	}
	if contribution.OpenPage != "" {
		if !validFeatureDestinationID(contribution.OpenPage) {
			panic(fmt.Sprintf("pluginrt: %s invalid page link %q", m.ID, contribution.OpenPage))
		}
		m.Opens = contribution.OpenPage
	}
	return m
}

func validFeatureDestinationID(id string) bool {
	for index, char := range id {
		switch {
		case 'a' <= char && char <= 'z', 'A' <= char && char <= 'Z', char == '_':
		case index > 0 && (char == '-' || '0' <= char && char <= '9'):
		default:
			return false
		}
	}
	return id != ""
}

func featureParts(declarations []sdk.FeaturePart) ([]Part, error) {
	parts := make([]Part, 0, len(declarations))
	seen := make(map[sdk.FeaturePart]bool, len(declarations))
	hasPage := false
	for _, declaration := range declarations {
		switch declaration.Slot {
		case sdk.FeatureNavigationPage:
			if hasPage {
				return nil, fmt.Errorf("multiple navigation pages")
			}
			hasPage = true
		case sdk.FeatureSettingsSection, sdk.FeatureInlinePart:
		default:
			return nil, fmt.Errorf("unknown slot %q", declaration.Slot)
		}
		if declaration.Key == "" || strings.TrimSpace(declaration.Key) != declaration.Key {
			return nil, fmt.Errorf("invalid key %q", declaration.Key)
		}
		if seen[declaration] {
			return nil, fmt.Errorf("duplicate %s key %q", declaration.Slot, declaration.Key)
		}
		seen[declaration] = true
		parts = append(parts, Part{Slot: string(declaration.Slot), Key: declaration.Key})
	}
	return parts, nil
}

func SchemaOf(settings []sdk.FeatureSetting) (*Schema, error) {
	schema := &Schema{Type: "object", Properties: make(map[string]*Property, len(settings))}
	for _, setting := range settings {
		if setting.Name == "" || strings.TrimSpace(setting.Name) != setting.Name || strings.TrimSpace(setting.Title) == "" {
			return nil, fmt.Errorf("invalid name or title for %q", setting.Name)
		}
		if schema.Properties[setting.Name] != nil {
			return nil, fmt.Errorf("duplicate setting %q", setting.Name)
		}
		property, err := featureProperty(setting)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", setting.Name, err)
		}
		schema.Order = append(schema.Order, setting.Name)
		schema.Properties[setting.Name] = property
	}
	return schema, nil
}

func featureProperty(setting sdk.FeatureSetting) (*Property, error) {
	switch setting.Kind {
	case sdk.FeatureSettingBoolean, sdk.FeatureSettingString:
		if setting.Minimum != nil || setting.Maximum != nil {
			return nil, fmt.Errorf("bounds require a numeric setting")
		}
	case sdk.FeatureSettingInteger, sdk.FeatureSettingNumber:
	default:
		return nil, fmt.Errorf("unsupported kind %q", setting.Kind)
	}
	property := &Property{
		Type: string(setting.Kind), Title: setting.Title, Description: setting.Description,
	}
	if setting.Minimum != nil {
		minimum := *setting.Minimum
		if math.IsNaN(minimum) || math.IsInf(minimum, 0) {
			return nil, fmt.Errorf("minimum must be finite")
		}
		property.Minimum = &minimum
	}
	if setting.Maximum != nil {
		maximum := *setting.Maximum
		if math.IsNaN(maximum) || math.IsInf(maximum, 0) || property.Minimum != nil && maximum < *property.Minimum {
			return nil, fmt.Errorf("invalid maximum")
		}
		property.Maximum = &maximum
	}
	var enum []any
	for _, value := range setting.Enum {
		normalized, err := property.value(value)
		if err != nil {
			return nil, fmt.Errorf("enum: %w", err)
		}
		enum = append(enum, normalized)
	}
	property.Enum = enum
	var err error
	property.Default, err = property.value(setting.Default)
	if err != nil {
		return nil, fmt.Errorf("default: %w", err)
	}
	return property, nil
}
