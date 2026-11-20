package omp

import (
	"bytes"
	"errors"
	"reflect"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
	"gopkg.in/yaml.v3"
)

const (
	ConfigFile       = "config.yml"
	CatalogFile      = "models.yml"
	LegacyModelsFile = "models.json"
	providerKey      = "providers.magpie"
)

type CatalogMode uint8

const (
	WriteCatalog CatalogMode = iota
	SyncCatalog
	DropCatalog
)

type SettingsOptions struct {
	Catalog CatalogMode
	Build   func(map[string]any) any
	Role    *RoleOptions
}

type ConfigChange struct {
	Set  []edit.KV
	Drop []string
}

func PlanConfig(options ConfigChange, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	var plan sdk.AgentPlan
	if len(settings.Files) != 1 || settings.Files[0].Path != ConfigFile {
		return plan, errors.New("OMP config action requires a config.yml snapshot")
	}
	file := settings.Files[0]
	body := file.Body
	var err error
	if len(options.Set) != 0 {
		body, err = edit.RewriteYAML(file.Path, body, options.Set...)
		if err != nil {
			return plan, err
		}
	}
	if len(options.Drop) != 0 {
		body, err = edit.RewriteYAMLWithout(file.Path, body, options.Drop...)
		if err != nil {
			return plan, err
		}
	}
	appendChange(&plan, file, body)
	return plan, nil
}

func PlanSettings(options SettingsOptions, settings sdk.ActionSettings) (sdk.AgentPlan, RoleResult, error) {
	var plan sdk.AgentPlan
	var result RoleResult
	var config, catalog, legacy sdk.ActionSettingFile
	for _, file := range settings.Files {
		switch file.Path {
		case ConfigFile:
			config = file
		case CatalogFile:
			catalog = file
		case LegacyModelsFile:
			legacy = file
		}
	}
	if catalog.Path == "" || options.Role != nil && config.Path == "" {
		return plan, result, errors.New("OMP settings action is missing a required snapshot")
	}
	var configBody []byte
	var routed bool
	if role := options.Role; role != nil {
		var err error
		configBody, options.Catalog, routed, result, err = rewriteRole(*role, config.Body)
		if err != nil {
			return plan, result, err
		}
	}
	catalogBody, err := rewriteCatalog(options, catalog, legacy)
	if err != nil {
		return plan, result, err
	}
	if options.Role != nil && !routed {
		appendChange(&plan, config, configBody)
	}
	appendChange(&plan, catalog, catalogBody)
	if options.Role != nil && routed {
		appendChange(&plan, config, configBody)
	}
	return plan, result, nil
}

func appendChange(plan *sdk.AgentPlan, file sdk.ActionSettingFile, body []byte) {
	if !bytes.Equal(edit.TrimBOM(file.Body), edit.TrimBOM(body)) {
		plan.Files = append(plan.Files, sdk.FileChange{Path: file.Path, Body: body, BeforeHash: file.BeforeHash})
	}
}

func rewriteCatalog(options SettingsOptions, catalog, legacy sdk.ActionSettingFile) ([]byte, error) {
	body := edit.TrimBOM(catalog.Body)
	switch options.Catalog {
	case DropCatalog:
		return edit.RewriteYAMLWithout(catalog.Path, body, providerKey)
	case WriteCatalog, SyncCatalog:
		if options.Build == nil {
			return nil, errors.New("OMP settings action is missing its provider builder")
		}
	default:
		return nil, errors.New("invalid OMP catalog operation")
	}
	if options.Catalog == WriteCatalog && !catalog.Exists && legacy.Exists {
		var err error
		body, err = edit.JSONAsYAML(legacy.Path, catalog.Path, legacy.Body, body)
		if err != nil {
			return nil, err
		}
	}
	var doc any
	parsed := yaml.Unmarshal(body, &doc) == nil
	root, _ := doc.(map[string]any)
	providers, _ := root["providers"].(map[string]any)
	current := providers["magpie"]
	if options.Catalog == SyncCatalog && (!parsed || current == nil) {
		return body, nil
	}
	existing, _ := current.(map[string]any)
	want := options.Build(existing)
	if options.Catalog == SyncCatalog {
		raw, err := yaml.Marshal(want)
		if err != nil {
			return nil, edit.AtFile(catalog.Path, err)
		}
		var value any
		if yaml.Unmarshal(raw, &value) == nil && reflect.DeepEqual(current, value) {
			return body, nil
		}
	}
	return edit.RewriteYAML(catalog.Path, body, edit.KV{Path: providerKey, Value: want})
}
