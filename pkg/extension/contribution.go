package extension

import (
	"fmt"
	"slices"
)

type ContributionPoint string

const (
	ContributionRouting ContributionPoint = "routing"
	ContributionSearch  ContributionPoint = "search"
)

// ContributionDescriptor registers one action under a versioned owner
// contract. The selected descriptor supplies its ID, action and metadata.
type ContributionDescriptor struct {
	Point   ContributionPoint          `json:"point"`
	Version int                        `json:"version"`
	Routing *RoutingStrategyDescriptor `json:"routing,omitempty"`
	Search  *SearchSourceDescriptor    `json:"search,omitempty"`
}

func (manifest Manifest) DeclaredRoutingStrategies() []RoutingStrategyDescriptor {
	strategies := slices.Clone(manifest.RoutingStrategies)
	for _, contribution := range manifest.Contributions {
		if contribution.Point == ContributionRouting && contribution.Version == 1 && contribution.Routing != nil {
			strategies = append(strategies, *contribution.Routing)
		}
	}
	return strategies
}

func (manifest Manifest) DeclaredSearchSources() []SearchSourceDescriptor {
	sources := slices.Clone(manifest.SearchSources)
	for _, contribution := range manifest.Contributions {
		if contribution.Point == ContributionSearch && contribution.Version == 1 && contribution.Search != nil {
			sources = append(sources, *contribution.Search)
		}
	}
	return sources
}

func (manifest Manifest) validateContributions() error {
	if len(manifest.Contributions) > 0 &&
		(manifest.Contract.Version.Minor < 16 || !manifest.Contract.Has(ModuleRPC) || manifest.Component != nil ||
			len(manifest.Contributions) > MaxRoutingStrategies+MaxSearchSources) {
		return fmt.Errorf("contributions require root SDK API 1.16 module-rpc within descriptor limits")
	}
	for _, contribution := range manifest.Contributions {
		switch {
		case contribution.Version != 1:
			return fmt.Errorf("unsupported %q contribution version %d", contribution.Point, contribution.Version)
		case contribution.Point == ContributionRouting && contribution.Routing != nil && contribution.Search == nil:
		case contribution.Point == ContributionSearch && contribution.Search != nil && contribution.Routing == nil:
		default:
			return fmt.Errorf("invalid contribution for point %q", contribution.Point)
		}
	}
	if len(manifest.Contributions) != 0 {
		manifest.RoutingStrategies = manifest.DeclaredRoutingStrategies()
		manifest.SearchSources = manifest.DeclaredSearchSources()
	}
	if err := manifest.validateRouting(); err != nil {
		return err
	}
	return manifest.validateSearchSources()
}
