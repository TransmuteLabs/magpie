package codex

import (
	"bytes"
	"errors"

	"github.com/yetone/magpie/internal/codexcat"
	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	ProfileConfigFile  = "magpie.config.toml"
	ProfileCatalogFile = "magpie-models.json"
	ProviderTablePath  = "model_providers.magpie"
)

type ProfilePlanOptions struct {
	Model   string
	BaseURL string
	Token   string
	Catalog []byte
}

func PlanCatalog(body []byte, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 2 || settings.Files[1].Path != ProfileCatalogFile ||
		settings.Files[0].Path != ProfileConfigFile && settings.Files[0].Path != GlobalConfigFile {
		return sdk.AgentPlan{}, errors.New("Codex catalog action requires config and catalog snapshots")
	}
	catalog := settings.Files[1]
	change := catalogChange(catalog, body)
	if catalog.Exists && bytes.Equal(edit.TrimBOM(catalog.Body), change.Body) {
		return sdk.AgentPlan{}, nil
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{change}}, nil
}

func PlanProfile(options ProfilePlanOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 2 {
		return sdk.AgentPlan{}, errors.New("Codex profile action requires a profile and catalog snapshot")
	}
	files := make(map[string]sdk.ActionSettingFile, len(settings.Files))
	for _, file := range settings.Files {
		files[file.Path] = file
	}
	config, hasConfig := files[ProfileConfigFile]
	catalog, hasCatalog := files[ProfileCatalogFile]
	if !hasConfig || !hasCatalog {
		return sdk.AgentPlan{}, errors.New("invalid Codex profile snapshot paths")
	}
	profile, err := edit.RewriteTOMLTable(config.Path, edit.TrimBOM(config.Body), ProviderTablePath, func(edit.TOMLTable) []edit.KV {
		return providerTable(options.BaseURL, options.Token)
	})
	if err != nil {
		return sdk.AgentPlan{}, err
	}
	profile, err = edit.RewriteTOMLTop(config.Path, profile,
		edit.KV{Path: "model_provider", Value: "magpie"},
		edit.KV{Path: "model_catalog_json", Value: ProfileCatalogFile},
		edit.KV{Path: "model", Value: options.Model},
	)
	if err != nil {
		return sdk.AgentPlan{}, err
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{
		catalogChange(catalog, options.Catalog),
		{Path: config.Path, BeforeHash: config.BeforeHash, Body: profile},
	}}, nil
}

func catalogChange(catalog sdk.ActionSettingFile, body []byte) sdk.FileChange {
	return sdk.FileChange{Path: catalog.Path, BeforeHash: catalog.BeforeHash, Body: codexcat.Keep(edit.TrimBOM(catalog.Body), body)}
}

// providerTable uses the OpenAI actor header required by Codex's built-in
// image tools. Inline lets the editor merge it into existing nested or
// dotted headers.
func providerTable(baseURL, token string) []edit.KV {
	return []edit.KV{
		{Path: "name", Value: "magpie"},
		{Path: "base_url", Value: baseURL},
		{Path: "wire_api", Value: "responses"},
		{Path: "experimental_bearer_token", Value: token},
		{Path: "http_headers", Value: edit.Inline{{Path: "x-openai-actor-authorization", Value: "magpie"}}},
	}
}
