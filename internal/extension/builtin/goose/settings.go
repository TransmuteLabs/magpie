package goose

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/lazyre"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	SettingsFile = "config.yaml"
	ProviderID   = "magpie"
	CatalogFile  = "custom_providers/" + ProviderID + ".json"
)

type Model struct {
	ID      string
	Context int
}

type CatalogOptions struct {
	URL, Key string
	Models   []Model
}

type ModelOptions struct {
	Selection string
	Catalog   func() CatalogOptions
}

// gooseThinks matches the model names goose sends its thinking level for on
// an OpenAI-compatible provider: OpenAI's reasoning models, as goose's
// is_openai_responses_model tells them (crates/goose-provider-types/src/
// formats/openai.rs). For any other model goose keeps the level to itself —
// thinking_effort is one of its own request params, never sent — so a
// level offered for it would change nothing; it is declared without
// reasoning rather than shown a picker that does nothing.
var gooseThinks = lazyre.New(`(?i)(?:^|[-/])(?:o\d+(?:$|-)|gpt-(?:5|6)(?:$|[-.]))`)

func PlanModel(options ModelOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	files := settings.Files
	withCatalog := options.Selection == "" || options.Catalog != nil
	if withCatalog && (len(files) != 2 || files[1].Path != CatalogFile) ||
		!withCatalog && len(files) != 1 || files[0].Path != SettingsFile {
		return sdk.AgentPlan{}, errors.New("Goose model plan needs config.yaml and any affected catalog")
	}
	config := files[0]
	body, err := rewriteModel(config, options.Selection)
	if err != nil {
		return sdk.AgentPlan{}, err
	}
	var plan sdk.AgentPlan
	if options.Catalog != nil {
		if err := planCatalog(&plan, files[1], options.Catalog(), false); err != nil {
			return sdk.AgentPlan{}, err
		}
	}
	appendChange(&plan, config, body)
	if options.Selection == "" && files[1].Exists {
		catalog := files[1]
		plan.Files = append(plan.Files, sdk.FileChange{Path: catalog.Path, BeforeHash: catalog.BeforeHash, Remove: true})
	}
	return plan, nil
}

func PlanSync(options func() CatalogOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	var plan sdk.AgentPlan
	if len(settings.Files) != 1 || settings.Files[0].Path != CatalogFile || options == nil {
		return plan, errors.New("Goose catalog sync needs its custom provider and available models")
	}
	catalog := settings.Files[0]
	if catalog.Exists {
		err := planCatalog(&plan, catalog, options(), true)
		return plan, err
	}
	return plan, nil
}

func rewriteModel(config sdk.ActionSettingFile, selection string) ([]byte, error) {
	raw := edit.TrimBOM(config.Body)
	if selection == "" {
		body, err := edit.RewriteYAMLWithoutTop(config.Path, raw, "active_provider", "GOOSE_PROVIDER", "GOOSE_MODEL")
		if err != nil {
			return nil, err
		}
		return edit.RewriteYAMLWithout(config.Path, body, "providers."+ProviderID)
	}
	provider, model, ok := strings.Cut(selection, "/")
	if !ok || provider == "" || model == "" {
		return nil, fmt.Errorf("expected provider/model, got %q", selection)
	}
	if !newLayout(raw) {
		return edit.RewriteYAMLTop(config.Path, raw,
			edit.KV{Path: "GOOSE_PROVIDER", Value: provider},
			edit.KV{Path: "GOOSE_MODEL", Value: model})
	}
	if strings.Contains(provider, ".") {
		return nil, fmt.Errorf("goose provider %q: a dot can't be written into its providers map", provider)
	}
	body, err := edit.RewriteYAML(config.Path, raw,
		edit.KV{Path: "active_provider", Value: provider},
		edit.KV{Path: "providers." + provider + ".enabled", Value: true},
		edit.KV{Path: "providers." + provider + ".model", Value: model},
		edit.KV{Path: "providers." + provider + ".configured", Value: true})
	if err != nil {
		return nil, err
	}
	return edit.RewriteYAMLWithoutTop(config.Path, body, "GOOSE_PROVIDER", "GOOSE_MODEL")
}

func newLayout(raw []byte) bool {
	var config map[string]any
	if yaml.Unmarshal(raw, &config) != nil {
		return false
	}
	_, providers := config["providers"]
	_, active := config["active_provider"]
	return providers || active
}

func planCatalog(plan *sdk.AgentPlan, file sdk.ActionSettingFile, options CatalogOptions, sync bool) error {
	value := providerJSON(options)
	raw := edit.TrimBOM(file.Body)
	var current map[string]any
	if json.Unmarshal(raw, &current) == nil && current != nil {
		if name, _ := current["display_name"].(string); strings.TrimSpace(name) != "" {
			value["display_name"] = name
		}
		if headers, _ := current["headers"].(map[string]any); headers != nil {
			value["headers"] = edit.MergeObjects(headers, value["headers"].(map[string]any), nil, nil)
		}
		value = edit.MergeObjects(current, value,
			[]string{"name", "engine", "api_key_env", "base_url", "requires_auth", "dynamic_models", "models", "headers"}, nil)
	}
	if sync && edit.SameJSON(string(raw), value) {
		return nil
	}
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return edit.AtFile(file.Path, err)
	}
	appendChange(plan, file, append(body, '\n'))
	return nil
}

func providerJSON(options CatalogOptions) map[string]any {
	models := make([]map[string]any, 0, len(options.Models))
	for _, model := range options.Models {
		entry := map[string]any{"name": model.ID, "reasoning": gooseThinks.MatchString(model.ID)}
		if model.Context > 0 {
			entry["context_limit"] = model.Context
		}
		models = append(models, entry)
	}
	provider := map[string]any{
		"name":                     ProviderID,
		"engine":                   "openai",
		"display_name":             "magpie",
		"description":              "Every model magpie has, through its local gateway",
		"api_key_env":              "",
		"base_url":                 options.URL,
		"headers":                  map[string]any{"Authorization": "Bearer " + options.Key},
		"requires_auth":            false,
		"skip_canonical_filtering": true,
		"models":                   models,
	}
	if len(models) > 0 {
		provider["dynamic_models"] = false
	}
	return provider
}

func appendChange(plan *sdk.AgentPlan, file sdk.ActionSettingFile, body []byte) {
	if !bytes.Equal(edit.TrimBOM(file.Body), body) {
		plan.Files = append(plan.Files, sdk.FileChange{Path: file.Path, Body: body, BeforeHash: file.BeforeHash})
	}
}
