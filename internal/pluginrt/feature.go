package pluginrt

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/routes"
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
	routing := maps.Clone(contribution.Routing)
	routingIDs := slices.Sorted(maps.Keys(routing))
	for _, id := range routingIDs {
		if err := routing[id].Validate(id); err != nil {
			panic(fmt.Errorf("pluginrt: %s routing: %w", m.ID, err))
		}
	}
	if activate := contribution.Activate; activate != nil || len(routingIDs) != 0 {
		m.Apply = func(ctx *Ctx) error {
			for _, id := range routingIDs {
				ctx.OnDispose(routes.Register(id, routing[id]))
			}
			if activate != nil {
				return activate(ctx)
			}
			return nil
		}
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
		if schema.Properties[setting.Name] != nil {
			return nil, fmt.Errorf("duplicate setting %q", setting.Name)
		}
		normalized, err := setting.Normalize()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", setting.Name, err)
		}
		schema.Order = append(schema.Order, setting.Name)
		schema.Properties[setting.Name] = &Property{
			Type: string(normalized.Kind), Title: normalized.Title, Description: normalized.Description,
			Default: normalized.Default, Enum: normalized.Enum, Minimum: normalized.Minimum, Maximum: normalized.Maximum,
		}
	}
	return schema, nil
}
