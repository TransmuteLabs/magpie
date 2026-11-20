package agent

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/yetone/magpie/internal/extension"
	builtinpi "github.com/yetone/magpie/internal/extension/builtin/pi"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// Pi's codemode calls classifier and image models (models.classify,
// models.generateImages; Pi's docs/models.md "Use classifier models" and
// "Use image models"), wangbinyq on Discord. Pi's models.json defines chat
// models only: its model definitions have no type, and a custom provider's
// classifier or image models come from an extension alone
// (docs/custom-provider.md, "Register mixed-operation models"). So magpie
// writes one, extensions/magpie.ts in Pi's agent folder, which Pi loads by
// itself. It lists magpie's decision models as provider magpie-systemone,
// on Pi's own System One client against magpie's /v1/systemone, and its
// image models as magpie-images, drawn by /v1/images/generations. It is
// there while models.json has providers.magpie, and gone with it.

//go:embed pi_extension.ts
var piExtensionSource string

// piExtensionPath is magpie's extension in Pi's agent folder dir.
func piExtensionPath(dir string) string {
	return filepath.Join(dir, builtinpi.SidecarFile)
}

// piExtensionModel is a model as the extension registers it.
type piExtensionModel struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Input []string `json:"input,omitempty"`
}

// piExtensionFor is the extension's source for a gateway at baseURL (its
// /v1) and key; "" when magpie has no decision or image model to list.
func piExtensionFor(baseURL, key string) string {
	ds := provider.OfferedDeciders(nil)
	for i := range ds {
		ds[i].Name = cmp.Or(ds[i].Name, ds[i].Model)
	}
	labels := provider.LabelsFor("pi", ds)
	classifiers := make([]piExtensionModel, 0, len(ds))
	for i, e := range ds {
		classifiers = append(classifiers, piExtensionModel{ID: e.ID, Name: labels[i]})
	}
	images := []piExtensionModel{}
	for _, m := range gateway.ImageModels() {
		in := []string{"text"}
		if m.Images {
			in = append(in, "image")
		}
		images = append(images, piExtensionModel{ID: m.ID, Name: m.Name, Input: in})
	}
	if len(classifiers) == 0 && len(images) == 0 {
		return ""
	}
	data, _ := json.Marshal(map[string]any{
		"baseUrl": baseURL, "apiKey": key,
		"classifiers": classifiers, "images": images,
	})
	return strings.Replace(piExtensionSource, "__MAGPIE__", string(data), 1)
}

func applyPiPlan[Options any](id, dir string, render func() string, options Options, planner func(Options, sdk.ActionSettings) (sdk.AgentPlan, error), files ...extension.LinkedActionFile) error {
	if id != "pi" {
		return extension.ApplyLinkedAgentPlan(options, planner, files...)
	}
	withSidecar := func(options Options, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
		inputs := settings.Files[:len(files)]
		plan, err := planner(options, sdk.ActionSettings{Files: inputs})
		if err != nil {
			return sdk.AgentPlan{}, err
		}
		for _, file := range inputs {
			if file.Path == builtinpi.CatalogFile {
				return builtinpi.WithSidecar(plan, file, settings.Files[len(files)], render)
			}
		}
		return plan, nil
	}
	sidecar := extension.LinkedActionFile{Name: builtinpi.SidecarFile, Path: piExtensionPath(dir), UnlinkName: true}
	return extension.ApplyLinkedAgentPlan(options, withSidecar, append(files, sidecar)...)
}
