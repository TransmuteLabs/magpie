package extension

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

type ContributionPoint string

const (
	ContributionRouting        ContributionPoint = "routing"
	ContributionSearch         ContributionPoint = "search"
	ContributionLibraryRule    ContributionPoint = "library-rule"
	ContributionLibraryCommand ContributionPoint = "library-command"
)

const (
	MaxLibraryRules              = 16
	MaxLibraryRuleBody           = 100_000
	MaxLibraryRulePaths          = 50
	MaxLibraryRulePath           = 500
	MaxLibraryCommands           = 16
	MaxLibraryCommandDescription = 1000
	MaxLibraryCommandPrompt      = 100_000
)

type LibraryRuleDescriptor struct {
	ID    string   `json:"id"`
	Body  string   `json:"body"`
	Paths []string `json:"paths,omitempty"`
}

type LibraryCommandDescriptor struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
	Prompt      string `json:"prompt"`
}

// ContributionDescriptor registers an action or resource under a versioned owner contract.
type ContributionDescriptor struct {
	Point          ContributionPoint          `json:"point"`
	Version        int                        `json:"version"`
	Routing        *RoutingStrategyDescriptor `json:"routing,omitempty"`
	Search         *SearchSourceDescriptor    `json:"search,omitempty"`
	LibraryRule    *LibraryRuleDescriptor     `json:"libraryRule,omitempty"`
	LibraryCommand *LibraryCommandDescriptor  `json:"libraryCommand,omitempty"`
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

func validateLibraryRule(rule LibraryRuleDescriptor) error {
	if !validID(rule.ID) || strings.TrimSpace(rule.Body) == "" || len(rule.Body) > MaxLibraryRuleBody ||
		strings.ContainsRune(rule.Body, 0) || len(rule.Paths) > MaxLibraryRulePaths {
		return fmt.Errorf("invalid Library rule %q", rule.ID)
	}
	for _, glob := range rule.Paths {
		if strings.TrimSpace(glob) == "" || len(glob) > MaxLibraryRulePath || strings.ContainsAny(glob, "\x00\r\n") {
			return fmt.Errorf("invalid glob in Library rule %q", rule.ID)
		}
	}
	return nil
}

func validateLibraryCommand(command LibraryCommandDescriptor) error {
	if !validID(command.ID) || utf8.RuneCountInString(command.Description) > MaxLibraryCommandDescription ||
		strings.ContainsRune(command.Description, 0) || strings.TrimSpace(command.Prompt) == "" ||
		len(command.Prompt) > MaxLibraryCommandPrompt || strings.ContainsRune(command.Prompt, 0) {
		return fmt.Errorf("invalid Library command %q", command.ID)
	}
	return nil
}

func (contribution ContributionDescriptor) descriptorCount() int {
	count := 0
	for _, present := range []bool{
		contribution.Routing != nil, contribution.Search != nil,
		contribution.LibraryRule != nil, contribution.LibraryCommand != nil,
	} {
		if present {
			count++
		}
	}
	return count
}

func (manifest Manifest) validateContributions() error {
	if len(manifest.Contributions) > 0 &&
		(manifest.Contract.Version.Minor < 16 || !manifest.Contract.Has(ModuleRPC) || manifest.Component != nil ||
			len(manifest.Contributions) > MaxRoutingStrategies+MaxSearchSources+MaxLibraryRules+MaxLibraryCommands) {
		return fmt.Errorf("contributions require root SDK API 1.16 module-rpc within descriptor limits")
	}
	rules := map[string]bool{}
	commands := map[string]bool{}
	for _, contribution := range manifest.Contributions {
		switch {
		case contribution.Version != 1:
			return fmt.Errorf("unsupported %q contribution version %d", contribution.Point, contribution.Version)
		case contribution.descriptorCount() != 1:
			return fmt.Errorf("invalid contribution for point %q", contribution.Point)
		case contribution.Point == ContributionRouting && contribution.Routing != nil:
		case contribution.Point == ContributionSearch && contribution.Search != nil:
		case contribution.Point == ContributionLibraryRule && contribution.LibraryRule != nil:
			rule := contribution.LibraryRule
			if manifest.Contract.Version.Minor < 17 || rules[rule.ID] || len(rules) >= MaxLibraryRules {
				return fmt.Errorf("Library rule %q requires API 1.17 and a unique ID within descriptor limits", rule.ID)
			}
			if err := validateLibraryRule(*rule); err != nil {
				return err
			}
			rules[rule.ID] = true
		case contribution.Point == ContributionLibraryCommand && contribution.LibraryCommand != nil:
			command := contribution.LibraryCommand
			if manifest.Contract.Version.Minor < 18 || commands[command.ID] || len(commands) >= MaxLibraryCommands {
				return fmt.Errorf("Library command %q requires API 1.18 and a unique ID within descriptor limits", command.ID)
			}
			if err := validateLibraryCommand(*command); err != nil {
				return err
			}
			commands[command.ID] = true
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
