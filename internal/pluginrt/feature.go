package pluginrt

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/gwbudget"
	"github.com/yetone/magpie/internal/gwmw"
	"github.com/yetone/magpie/internal/gwtool"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/routes"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// RegisterFeature registers a linked SDK contribution under the existing
// built-in manifest's identity and lifecycle.
func RegisterFeature(contribution sdk.FeatureContribution, bind ...func(*Ctx)) {
	register(featureManifest(contribution, bind...))
}

type RoutingFeature = sdk.LinkedRoutingFeature[*routes.Catalog]

func RegisterRoutingFeature(contribution RoutingFeature) {
	register(featureManifestWithRouting(contribution.FeatureContribution, contribution.Strategies))
}

type GatewayFeature = sdk.LinkedGatewayFeature[access.Identity, usage.Record]

func RegisterGatewayFeature(contribution GatewayFeature) {
	register(gatewayFeatureManifest(contribution))
}

func gatewayFeatureManifest(contribution GatewayFeature) Manifest {
	feature := contribution.FeatureContribution
	if contribution.Budget != nil || contribution.Observer != nil {
		activate := feature.Activate
		feature.Activate = func(lifetime sdk.FeatureLifetime) error {
			if contribution.Budget != nil {
				lifetime.OnDispose(gwbudget.Limit(contribution.Budget))
			}
			if contribution.Observer != nil {
				observer := contribution.Observer()
				if observer == nil {
					return fmt.Errorf("pluginrt: %s returned no usage observer", feature.ID)
				}
				lifetime.OnDispose(usage.Observe(observer))
			}
			if activate != nil {
				return activate(lifetime)
			}
			return nil
		}
	}
	return featureManifest(feature)
}

func featureManifest(contribution sdk.FeatureContribution, bind ...func(*Ctx)) Manifest {
	return featureManifestWithRouting(contribution, nil, bind...)
}

func featureManifestWithRouting(contribution sdk.FeatureContribution, strategies func(*routes.Catalog) []sdk.LinkedRoutingRegistration, bind ...func(*Ctx)) Manifest {
	m := Manifest{
		ID: contribution.ID, API: contribution.API,
		Requires: slices.Clone(contribution.Requires), Essential: contribution.Essential,
		Name: contribution.Name, Sub: contribution.Sub, One: contribution.One, Many: contribution.Many,
		InUse: contribution.InUse, Client: slices.Clone(contribution.Client),
	}
	routing := make([]sdk.LinkedRoutingRegistration, 0, len(contribution.Routing))
	for _, id := range slices.Sorted(maps.Keys(contribution.Routing)) {
		strategy := contribution.Routing[id]
		if err := strategy.Validate(id); err != nil {
			panic(fmt.Errorf("pluginrt: %s routing: %w", m.ID, err))
		}
		routing = append(routing, sdk.LinkedRoutingRegistration{ID: id, Name: id, Strategy: strategy})
	}
	tools := slices.Clone(contribution.Tools)
	seenTools := make(map[sdk.LinkedGatewayTool]bool, len(tools))
	for _, tool := range tools {
		if tool != sdk.LinkedWebSearch && tool != sdk.LinkedPluginTools {
			panic(fmt.Errorf("pluginrt: %s unknown gateway tool %q", m.ID, tool))
		}
		if seenTools[tool] {
			panic(fmt.Errorf("pluginrt: %s duplicate gateway tool %q", m.ID, tool))
		}
		seenTools[tool] = true
	}
	middleware, masker := contribution.Middleware, contribution.Masker
	groups := contribution.RoutingGroups
	if activate := contribution.Activate; activate != nil || len(bind) != 0 || len(routing) != 0 || strategies != nil || groups || middleware != nil || masker != nil || len(tools) != 0 {
		m.Apply = func(ctx *Ctx) error {
			catalog := ctx.RoutingCatalog()
			declared := routing
			if strategies != nil {
				declared = append(slices.Clone(routing), strategies(catalog)...)
			}
			if len(declared) != 0 {
				remove, err := catalog.RegisterLinked(declared)
				if err != nil {
					return fmt.Errorf("pluginrt: %s routing: %w", m.ID, err)
				}
				ctx.OnDispose(remove)
			}
			if groups {
				ctx.OnDispose(provider.ServeGroups())
			}
			if middleware != nil {
				ctx.OnDispose(gwmw.Use(middleware))
			}
			if masker != nil {
				ctx.OnDispose(gwmw.Mask(masker))
			}
			for _, tool := range tools {
				ctx.OnDispose(gwtool.Serve(string(tool)))
			}
			for _, configure := range bind {
				configure(ctx)
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
