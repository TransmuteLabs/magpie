package agy

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	SettingsFile = "settings.json"
	OverlayFile  = "magpie-settings.json"
	ModelsKey    = "customModelsConfig.customModels"
	modelPrefix  = "magpie/"
)

var levels = []string{"low", "medium", "high"}

type Model struct {
	ID      string
	Efforts []string
}

type SettingsOptions struct {
	Sync    bool
	Routed  bool
	Model   string
	Restore func(legacy bool) (model, provider string)
	Catalog func() []Model
}

type SettingsResult struct {
	Routed      bool
	DropOverlay bool
	Legacy      bool
}

func BaseModel(model string) string {
	i := strings.LastIndex(model, ":")
	if i >= 0 && strings.HasPrefix(model, modelPrefix) && slices.Contains(levels, model[i+1:]) {
		return model[:i]
	}
	return model
}

func CustomModels(body []byte) map[string]json.RawMessage {
	return customModels(gjson.ParseBytes(jsonc.ToJSON(edit.TrimBOM(body))))
}

func customModels(root gjson.Result) map[string]json.RawMessage {
	models := map[string]json.RawMessage{}
	root.Get(ModelsKey).ForEach(func(key, value gjson.Result) bool {
		models[key.String()] = json.RawMessage(value.Raw)
		return true
	})
	return models
}

func PlanSettings(options SettingsOptions, snapshot sdk.ActionSettings) (sdk.AgentPlan, SettingsResult, error) {
	files := snapshot.Files
	if len(files) != 2 || files[0].Path != SettingsFile || files[1].Path != OverlayFile {
		return sdk.AgentPlan{}, SettingsResult{}, errors.New("Antigravity CLI settings plan requires native and overlay settings")
	}
	native, overlay := files[0], files[1]
	nativeJSON := jsonc.ToJSON(edit.TrimBOM(native.Body))
	root := gjson.ParseBytes(nativeJSON)
	nativeModel := root.Get("model").String()
	overlayModel := gjson.GetBytes(jsonc.ToJSON(edit.TrimBOM(overlay.Body)), "model").String()
	nativeRouted := strings.HasPrefix(nativeModel, modelPrefix)
	overlayRouted := strings.HasPrefix(overlayModel, modelPrefix)
	model, routed := options.Model, options.Routed
	if options.Sync {
		model = overlayModel
		if nativeRouted {
			model = nativeModel
		}
		routed = strings.HasPrefix(model, modelPrefix)
	}
	result := SettingsResult{
		Routed:      routed,
		DropOverlay: !options.Sync && !routed && overlayRouted,
		Legacy:      nativeRouted && !overlayRouted,
	}
	var set []edit.KV
	var remove []string
	put := func(key, value string) {
		if value == "" {
			remove = append(remove, key)
		} else {
			set = append(set, edit.KV{Path: key, Value: value})
		}
	}
	var own map[string]json.RawMessage
	if result.Legacy || routed {
		if len(bytes.TrimSpace(nativeJSON)) != 0 && (!gjson.ValidBytes(nativeJSON) || !root.IsObject()) {
			return sdk.AgentPlan{}, SettingsResult{}, edit.AtFile(native.Path, errors.New("invalid settings object"))
		}
		own = customModels(root)
		for key := range own {
			if strings.HasPrefix(key, modelPrefix) {
				delete(own, key)
			}
		}
	}
	var originalModel, originalProvider string
	if nativeRouted {
		originalModel, originalProvider = options.Restore(result.Legacy)
	}
	if result.Legacy {
		put("modelProvider", originalProvider)
		if len(own) == 0 {
			remove = append(remove, ModelsKey)
		} else {
			set = append(set, edit.KV{Path: ModelsKey, Value: own})
		}
	}
	switch {
	case !options.Sync && !routed && model != "":
		put("model", model)
	case nativeRouted:
		put("model", originalModel)
	}

	var plan sdk.AgentPlan
	if len(set)+len(remove) != 0 {
		body, err := edit.PatchJSON(native.Body, set, remove)
		if err == nil && result.Legacy && len(own) == 0 &&
			gjson.GetBytes(jsonc.ToJSON(edit.TrimBOM(body)), "customModelsConfig").Raw == "{}" {
			body, err = edit.PatchJSON(body, nil, []string{"customModelsConfig"})
		}
		if err != nil {
			return sdk.AgentPlan{}, SettingsResult{}, edit.AtFile(native.Path, err)
		}
		if !bytes.Equal(native.Body, body) {
			plan.Files = append(plan.Files, sdk.FileChange{Path: native.Path, BeforeHash: native.BeforeHash, Body: body})
		}
	}
	if routed {
		models := make(map[string]any, len(own))
		for key, value := range own {
			models[key] = value
		}
		for _, entry := range options.Catalog() {
			key := modelPrefix + entry.ID
			models[key] = customModel(entry.ID, "")
			var supported []string
			for _, level := range levels {
				if slices.ContainsFunc(entry.Efforts, func(effort string) bool { return strings.EqualFold(effort, level) }) {
					supported = append(supported, level)
				}
			}
			if len(supported) >= 2 {
				for _, level := range supported {
					models[key+":"+level] = customModel(entry.ID, level)
				}
			}
		}
		body, err := edit.PatchJSON(overlay.Body, []edit.KV{
			{Path: "modelProvider", Value: "gemini"}, {Path: "model", Value: model}, {Path: ModelsKey, Value: models},
		}, nil)
		if err != nil {
			return sdk.AgentPlan{}, SettingsResult{}, edit.AtFile(overlay.Path, err)
		}
		if !bytes.Equal(overlay.Body, body) {
			plan.Files = append(plan.Files, sdk.FileChange{Path: overlay.Path, BeforeHash: overlay.BeforeHash, Body: body})
		}
	}
	return plan, result, nil
}

func customModel(model, level string) map[string]string {
	entry := map[string]string{"apiProvider": "API_PROVIDER_GOOGLE_GEMINI", "modelName": model}
	if level != "" {
		entry["thinkingLevel"] = level
	}
	return entry
}
