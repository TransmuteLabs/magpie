package codex

import (
	"bytes"
	"errors"

	"github.com/pelletier/go-toml/v2"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const DesktopCatalogFile = "magpie-desktop-models.json"

var ErrDesktopCatalogReferenceChanged = errors.New("Codex desktop catalog reference changed")

type DesktopCatalogOptions struct {
	CurrentReference string
	Reference        string
	Catalog          []byte
	Cleanup          *DesktopCatalogCleanup
}

type DesktopCatalogCleanup struct {
	Unname      bool
	MissingOnly bool
}

func PlanDesktopCatalog(options DesktopCatalogOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 2 || settings.Files[0].Path != GlobalConfigFile ||
		settings.Files[1].Path != DesktopCatalogFile {
		return sdk.AgentPlan{}, errors.New("Codex desktop action requires config and catalog snapshots")
	}
	config, catalog := settings.Files[0], settings.Files[1]
	raw := edit.TrimBOM(config.Body)
	var current struct {
		Reference string `toml:"model_catalog_json"`
	}
	if err := toml.Unmarshal(edit.TOMLWithout(raw, "projects"), &current); err != nil {
		return sdk.AgentPlan{}, edit.AtFile(config.Path, errors.Join(ErrDesktopCatalogReferenceChanged, err))
	}
	if current.Reference != options.CurrentReference {
		return sdk.AgentPlan{}, ErrDesktopCatalogReferenceChanged
	}
	if cleanup := options.Cleanup; cleanup != nil {
		if !cleanup.Unname || cleanup.MissingOnly && catalog.Exists {
			return sdk.AgentPlan{}, nil
		}
		body, err := edit.RewriteTOMLWithoutTop(config.Path, raw, "model_catalog_json")
		if err != nil {
			return sdk.AgentPlan{}, err
		}
		return sdk.AgentPlan{Files: []sdk.FileChange{
			{Path: config.Path, Body: body, BeforeHash: config.BeforeHash},
		}}, nil
	}
	var result sdk.AgentPlan
	if !catalog.Exists || !bytes.Equal(edit.TrimBOM(catalog.Body), options.Catalog) {
		result.Files = append(result.Files, sdk.FileChange{
			Path: catalog.Path, Body: options.Catalog, BeforeHash: catalog.BeforeHash,
		})
	}
	if current.Reference == "" {
		body, err := edit.RewriteTOMLTop(config.Path, raw, edit.KV{Path: "model_catalog_json", Value: options.Reference})
		if err != nil {
			return sdk.AgentPlan{}, err
		}
		result.Files = append(result.Files, sdk.FileChange{
			Path: config.Path, Body: body, BeforeHash: config.BeforeHash,
		})
	}
	return result, nil
}
