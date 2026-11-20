package grok

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	SettingsFile     = "config.toml"
	ModelTablePrefix = `model."magpie/`
	effortKey        = "default_reasoning_effort"
)

type Action int

const (
	SelectModel Action = iota
	SetEffort
	SyncCatalog
)

type Catalog struct {
	URL, Key string
	Models   []catalog.Model
	Working  func(int) int
}

type Options struct {
	Action            Action
	Name, Value       string
	GatewayModel      string
	Windows, Percents string
	Catalog           func() Catalog
}

type Recovery struct {
	Windows, Percents string
	Forget            bool
}

var efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

func Plan(options Options, settings sdk.ActionSettings) (sdk.AgentPlan, Recovery, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != SettingsFile {
		return sdk.AgentPlan{}, Recovery{}, errors.New("Grok settings plan needs config.toml")
	}
	file := settings.Files[0]
	raw := edit.TrimBOM(file.Body)
	body, recovery, err := rewrite(options, file.Path, raw)
	if err != nil {
		return sdk.AgentPlan{}, Recovery{}, err
	}
	if bytes.Equal(body, raw) {
		return sdk.AgentPlan{}, recovery, nil
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: file.Path, Body: body, BeforeHash: file.BeforeHash}}}, recovery, nil
}

func rewrite(options Options, path string, raw []byte) ([]byte, Recovery, error) {
	if options.Action == SetEffort {
		value := options.Value
		if value != "" && !slices.Contains(efforts, value) {
			return nil, Recovery{}, fmt.Errorf("%s takes no effort %q: %s", options.Name, value, strings.Join(efforts, ", "))
		}
		body, err := rewriteEffort(path, raw, value)
		return body, Recovery{}, err
	}
	if options.Action == SyncCatalog {
		return syncCatalog(options, path, raw)
	}
	_, models, err := edit.ReadTOMLTableSnapshot(path, raw, "models")
	if err != nil {
		return nil, Recovery{}, err
	}
	selection := options.Value
	_, features, err := edit.ReadTOMLTableSnapshot(path, raw, "features")
	if err != nil {
		return nil, Recovery{}, err
	}
	if selection == "" {
		body, err := edit.RewriteTOMLWithoutKey(path, raw, "models", "default")
		if err != nil {
			return nil, Recovery{}, err
		}
		body, err = edit.RewriteTOMLWithoutKey(path, body, "features", "campaigns")
		if err != nil {
			return nil, Recovery{}, err
		}
		return withoutCatalog(path, body)
	}
	var body []byte
	var recovery Recovery
	if options.GatewayModel != "" {
		body, recovery, err = rewriteCatalog(options, path, raw, models.Scalar(effortKey))
	} else {
		body, recovery, err = withoutCatalog(path, raw)
	}
	if err != nil {
		return nil, Recovery{}, err
	}
	body, err = suppressCampaigns(path, body, features.Scalar("campaigns"))
	if err != nil {
		return nil, Recovery{}, err
	}
	body, err = edit.RewriteTOMLKey(path, body, "models", "default", selection)
	return body, recovery, err
}

func syncCatalog(options Options, path string, raw []byte) ([]byte, Recovery, error) {
	wired, err := edit.HasTOMLTablePrefix(path, raw, ModelTablePrefix)
	if err != nil {
		return nil, Recovery{}, err
	}
	if !wired {
		return raw, Recovery{}, nil
	}
	_, models, err := edit.ReadTOMLTableSnapshot(path, raw, "models")
	if err != nil {
		return nil, Recovery{}, err
	}
	if models.Scalar("default") != "" {
		_, features, err := edit.ReadTOMLTableSnapshot(path, raw, "features")
		if err != nil {
			return nil, Recovery{}, err
		}
		raw, err = suppressCampaigns(path, raw, features.Scalar("campaigns"))
		if err != nil {
			return nil, Recovery{}, err
		}
	}
	return rewriteCatalog(options, path, raw, "")
}

func rewriteEffort(path string, body []byte, value string) ([]byte, error) {
	if value == "" {
		return edit.RewriteTOMLWithoutKey(path, body, "models", effortKey)
	}
	return edit.RewriteTOMLKey(path, body, "models", effortKey, value)
}

func suppressCampaigns(path string, body []byte, current string) ([]byte, error) {
	if current == "false" {
		return body, nil
	}
	return edit.RewriteTOMLKey(path, body, "features", "campaigns", false)
}

func withoutCatalog(path string, raw []byte) ([]byte, Recovery, error) {
	body, err := edit.RewriteTOMLTablesMatching(path, raw, nil, []string{ModelTablePrefix}, nil)
	return body, Recovery{Forget: true}, err
}

func rewriteCatalog(options Options, path string, raw []byte, effort string) ([]byte, Recovery, error) {
	if options.Catalog == nil {
		return nil, Recovery{}, errors.New("Grok settings plan needs the visible catalog")
	}
	had, err := ReadModelSettings(raw)
	if err != nil {
		return nil, Recovery{}, edit.AtFile(path, err)
	}
	var previousWindows, previousPercents map[string]int
	_ = json.Unmarshal([]byte(options.Windows), &previousWindows)
	_ = json.Unmarshal([]byte(options.Percents), &previousPercents)
	visible := options.Catalog()
	tables, windows, percents := modelTables(visible, had, previousWindows, previousPercents)
	body, err := edit.RewriteTOMLTablesMatching(path, raw, nil, []string{ModelTablePrefix}, tables)
	if err != nil {
		return nil, Recovery{}, err
	}
	if effort != "" {
		if allowed := catalog.Efforts(visible.Models, options.GatewayModel); allowed != nil && !slices.Contains(allowed, effort) {
			nearest := catalog.NearestEffort(effort, allowed)
			if !slices.Contains(allowed, nearest) {
				nearest = ""
			}
			body, err = rewriteEffort(path, body, nearest)
			if err != nil {
				return nil, Recovery{}, err
			}
		}
	}
	var recovery Recovery
	if options.Windows == "" || !maps.Equal(windows, previousWindows) {
		body, _ := json.Marshal(windows)
		recovery.Windows = string(body)
	}
	if options.Percents == "" || !maps.Equal(percents, previousPercents) {
		body, _ := json.Marshal(percents)
		recovery.Percents = string(body)
	}
	return body, recovery, nil
}
