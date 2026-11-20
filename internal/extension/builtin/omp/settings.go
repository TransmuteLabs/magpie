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

type RoleChange struct {
	Name                string
	Value               string
	UpdateEnabledModels func(*yaml.Node) (bool, bool)
}

type SettingsOptions struct {
	Catalog CatalogMode
	Build   func(map[string]any) any
	Role    *RoleChange
}

func PlanSettings(options SettingsOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	var plan sdk.AgentPlan
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
		return plan, errors.New("OMP settings action is missing a required snapshot")
	}
	changed := func(file sdk.ActionSettingFile, body []byte) {
		if !bytes.Equal(edit.TrimBOM(file.Body), edit.TrimBOM(body)) {
			plan.Files = append(plan.Files, sdk.FileChange{Path: file.Path, Body: body, BeforeHash: file.BeforeHash})
		}
	}
	body, err := rewriteCatalog(options, catalog, legacy)
	if err != nil {
		return sdk.AgentPlan{}, err
	}
	changed(catalog, body)
	if role := options.Role; role != nil {
		body, err := edit.RewriteYAML(config.Path, config.Body, edit.KV{Path: "modelRoles." + role.Name, Value: role.Value})
		if err != nil {
			return sdk.AgentPlan{}, err
		}
		if role.UpdateEnabledModels != nil {
			body, err = edit.RewriteYAMLNode(config.Path, body, "enabledModels", role.UpdateEnabledModels)
			if err != nil {
				return sdk.AgentPlan{}, err
			}
		}
		changed(config, body)
	}
	return plan, nil
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
