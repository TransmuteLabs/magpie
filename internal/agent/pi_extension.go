package agent

import (
	"bytes"
	"cmp"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
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

// piExtensionMark opens the file magpie writes. A file at that path
// without it is the user's own, and is never written over or removed.
const piExtensionMark = "// magpie: written by magpie"

// piExtensionPath is magpie's extension in Pi's agent folder dir.
func piExtensionPath(dir string) string {
	return filepath.Join(dir, "extensions", "magpie.ts")
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

func piExtensionEdit(dir string, catalog wiringFileEdit, render func() string) (wiringFileEdit, error) {
	path := piExtensionPath(dir)
	current, err := readWiringFile(path)
	if err != nil {
		return wiringFileEdit{}, edit.PreserveOnRollback(path, err)
	}
	sidecar := wiringFileEdit{path: path, before: current}
	if current.info != nil && !bytes.HasPrefix(edit.TrimBOM(current.body), []byte(piExtensionMark)) {
		return sidecar, nil
	}
	body := catalog.body
	if body == nil {
		body = catalog.before.body
	}
	var want string
	if gjson.GetBytes(jsonc.ToJSON(edit.TrimBOM(body)), "providers."+magpieID).Exists() {
		want = render()
	}
	if want != "" && want != string(current.body) {
		sidecar.body = []byte(want)
	} else if want == "" && current.info != nil {
		name, err := readWiringName(path)
		if err != nil {
			return wiringFileEdit{}, edit.PreserveOnRollback(path, err)
		}
		sidecar.unlink, sidecar.name = true, &name
	}
	return sidecar, nil
}
