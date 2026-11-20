package stepcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/edit"
	builtinpi "github.com/yetone/magpie/internal/extension/builtin/pi"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	ConfigFile      = "config.toml"
	CatalogFile     = "models.json"
	scopeKey        = "enabledModels"
	providerKey     = "providers.magpie"
	modelCatalogKey = providerKey + ".models"
)

type Settings struct {
	Provider *string   `toml:"defaultProvider"`
	Model    *string   `toml:"defaultModel"`
	Scope    *[]string `toml:"enabledModels"`
}

func ReadSettings(body []byte) (Settings, error) {
	var settings Settings
	if len(bytes.TrimSpace(body)) == 0 {
		return settings, nil
	}
	err := toml.Unmarshal(body, &settings)
	return settings, err
}

func (settings Settings) Startup() string {
	var model string
	if settings.Model != nil && *settings.Model != "" {
		model = *settings.Model
		if settings.Provider != nil && *settings.Provider != "" {
			model = *settings.Provider + "/" + model
		}
	}
	if settings.Scope == nil || len(*settings.Scope) == 0 {
		return model
	}
	scope := *settings.Scope
	if model != "" && slices.ContainsFunc(scope, func(pattern string) bool { return builtinpi.Takes(pattern, model) }) {
		return model
	}
	first := builtinpi.Bare(strings.TrimSpace(scope[0]))
	if strings.ContainsAny(first, "*?[") || !strings.Contains(first, "/") {
		return model
	}
	return first
}

func ScopeValue(patterns []string) (edit.Raw, error) {
	body, err := toml.Marshal(map[string]any{scopeKey: patterns})
	if err != nil {
		return "", err
	}
	_, value, ok := strings.Cut(string(body), "=")
	if !ok {
		return "", fmt.Errorf("unexpected toml output for %s: %s", scopeKey, body)
	}
	return edit.Raw(strings.TrimSpace(value)), nil
}

func ReadCatalog(body []byte) (gjson.Result, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return gjson.Result{}, nil
	}
	normalized := jsonc.ToJSON(body)
	if !gjson.ValidBytes(normalized) {
		return gjson.Result{}, errors.New("invalid JSON")
	}
	catalog := gjson.ParseBytes(normalized)
	if !catalog.IsObject() {
		return gjson.Result{}, errors.New("root must be a JSON object")
	}
	if providers := catalog.Get("providers"); providers.Exists() {
		if !providers.IsObject() {
			return gjson.Result{}, errors.New("'providers' must be a JSON object")
		}
		if magpie := providers.Get("magpie"); magpie.Exists() && !magpie.IsObject() {
			return gjson.Result{}, fmt.Errorf("'%s' must be a JSON object", providerKey)
		}
	}
	return catalog, nil
}

type ScopeOptions struct {
	Catalog func(current string) (any, bool)
	Update  func(Settings, []string) ([]string, bool)
}

func PlanScope(options ScopeOptions, snapshot sdk.ActionSettings) (sdk.AgentPlan, error) {
	files := snapshot.Files
	syncing := options.Catalog != nil
	if len(files) == 0 || files[0].Path != ConfigFile ||
		syncing && (len(files) != 2 || files[1].Path != CatalogFile) || !syncing && len(files) != 1 {
		return sdk.AgentPlan{}, errors.New("StepCode scope action requires config.toml and, on sync, models.json")
	}
	var plan sdk.AgentPlan
	var offered []string
	if syncing {
		catalog := files[1]
		after, err := ReadCatalog(catalog.Body)
		if err != nil {
			return sdk.AgentPlan{}, edit.AtFile(catalog.Path, err)
		}
		if current := after.Get(providerKey); current.Exists() {
			next, changed := options.Catalog(current.Raw)
			if changed {
				body, err := edit.PatchJSON(catalog.Body, []edit.KV{{Path: providerKey, Value: next}}, nil)
				if err != nil {
					return sdk.AgentPlan{}, edit.AtFile(catalog.Path, err)
				}
				plan.Files = append(plan.Files, sdk.FileChange{Path: catalog.Path, BeforeHash: catalog.BeforeHash, Body: body})
				after = gjson.ParseBytes(jsonc.ToJSON(body))
			}
		}
		var models []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(after.Get(modelCatalogKey).Raw), &models) != nil {
			return plan, nil
		}
		for _, model := range models {
			offered = append(offered, model.ID)
		}
	}
	config := files[0]
	settings, err := ReadSettings(config.Body)
	if err != nil {
		if syncing {
			return plan, nil
		}
		return sdk.AgentPlan{}, edit.AtFile(config.Path, err)
	}
	patterns, changed := options.Update(settings, offered)
	if !changed {
		return plan, nil
	}
	raw := edit.TrimBOM(config.Body)
	var body []byte
	if len(patterns) == 0 {
		body, err = edit.RewriteTOMLWithoutTop(config.Path, raw, scopeKey)
	} else {
		value, valueErr := ScopeValue(patterns)
		if valueErr != nil {
			return sdk.AgentPlan{}, edit.AtFile(config.Path, valueErr)
		}
		body, err = edit.RewriteTOMLTopPreserving(config.Path, raw, edit.KV{Path: scopeKey, Value: value})
	}
	if err != nil {
		return sdk.AgentPlan{}, err
	}
	plan.Files = append(plan.Files, sdk.FileChange{Path: config.Path, BeforeHash: config.BeforeHash, Body: body})
	return plan, nil
}
