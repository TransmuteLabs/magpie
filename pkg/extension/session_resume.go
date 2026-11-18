package extension

import (
	"errors"
	"slices"
	"strings"
	"unicode"
)

type SessionResumeRecipe struct {
	Agent string   `json:"agent"`
	Args  []string `json:"args"`
}

func (recipe *SessionResumeRecipe) Clone() *SessionResumeRecipe {
	if recipe == nil {
		return nil
	}
	cloned := *recipe
	cloned.Args = slices.Clone(recipe.Args)
	return &cloned
}

func ValidateSessionResume(manifest Manifest, owner, mode string, recipe *SessionResumeRecipe) error {
	if mode != "agent" {
		if recipe != nil || mode != "none" && mode != "native" {
			return errors.New("invalid session resume mode or unexpected agent recipe")
		}
		return nil
	}
	if manifest.Contract.Version.Minor < 10 || !manifest.Contract.Has(AgentWiring) ||
		recipe == nil || !validID(recipe.Agent) {
		return errors.New("agent-backed session resume requires API 1.10 and an owned launch recipe")
	}
	owned := slices.ContainsFunc(manifest.Agents, func(agent AgentDescriptor) bool {
		return agent.ID == recipe.Agent && agent.Launch != nil &&
			(agent.ID == owner || agent.Profile != nil && agent.Profile.Target == owner)
	})
	if !owned {
		return errors.New("session resume agent must belong to the source's own package and native owner")
	}
	return recipe.validateArguments()
}

func (recipe SessionResumeRecipe) validateArguments() error {
	if len(recipe.Args) == 0 || len(recipe.Args) > 32 {
		return errors.New("session resume requires bounded arguments")
	}
	placeholders := 0
	for _, argument := range recipe.Args {
		if argument == "{sessionId}" {
			placeholders++
		} else if argument == "" || len(argument) > 1024 || strings.ContainsAny(argument, "{}") ||
			strings.ContainsFunc(argument, unicode.IsControl) {
			return errors.New("session resume accepts bounded literal argv and one session identity placeholder")
		}
	}
	if placeholders != 1 {
		return errors.New("session resume requires exactly one session identity argument")
	}
	return nil
}

func (recipe SessionResumeRecipe) Arguments(id string) ([]string, error) {
	if err := recipe.validateArguments(); err != nil {
		return nil, err
	}
	if !validSessionSourceID(id) || strings.HasPrefix(id, "-") || strings.ContainsFunc(id, unicode.IsControl) {
		return nil, errors.New("invalid session resume identity")
	}
	arguments := slices.Clone(recipe.Args)
	for index, argument := range arguments {
		if argument == "{sessionId}" {
			arguments[index] = id
		}
	}
	return arguments, nil
}
