package qoder

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/edit"
)

const SettingsFile = "settings.json"
const ProviderPath = "providers.magpie"
const ModelPath = "model.name"

const preferencesPath = "model.preferences"
const fallbackEffortPath = "model.reasoningEffort"

type Action uint8

const (
	SelectModel Action = iota
	SelectEffort
	SyncCatalog
)

type Options struct {
	Action   Action
	Value    string
	Previous string
	ModelRef func(string) (string, bool)
	OwnKey   func(string) bool
	Provider func(ref string) map[string]any
}

type Recovery struct {
	Save     bool
	Previous string
	Restored bool
}

func Patch(options Options, raw []byte) ([]byte, Recovery, error) {
	clean := jsonc.ToJSON(edit.TrimBOM(raw))
	config := gjson.ParseBytes(clean)
	if len(bytes.TrimSpace(clean)) > 0 && (!json.Valid(clean) || !config.IsObject()) {
		return nil, Recovery{}, errors.New("invalid JSON/JSONC settings object")
	}
	current := config.Get(ProviderPath)
	model := config.Get(ModelPath).String()
	prefs := config.Get(preferencesPath).String()
	var set []edit.KV
	var del []string
	var recovery Recovery
	switch options.Action {
	case SyncCatalog:
		ref, routed := options.ModelRef(model)
		if routed && current.Exists() {
			next := keptProvider(current.String(), options.Provider(ref))
			if !edit.SameJSON(current.Raw, next) {
				set = append(set, edit.KV{Path: ProviderPath, Value: next})
			}
		}
	case SelectEffort:
		set, del = appendEffort(set, prefs, model, options.Value)
	case SelectModel:
		selection := options.Value
		owned := options.OwnKey(current.Get("apiKey").String())
		if ref, routed := options.ModelRef(selection); routed {
			_, alreadyRouted := options.ModelRef(model)
			recovery = Recovery{Save: !owned || !alreadyRouted, Previous: model}
			set = append(set, edit.KV{Path: ProviderPath, Value: keptProvider(current.String(), options.Provider(ref))})
			was := Effort(prefs, model, config.Get(fallbackEffortPath).String())
			if was != "" && modelEffort(prefs, selection) == "" {
				set, del = appendEffort(set, prefs, selection, was)
			}
		} else if owned {
			del = append(del, ProviderPath)
			if selection == "" {
				selection = options.Previous
				recovery.Restored = true
			}
		}
		if selection == "" {
			del = append(del, ModelPath)
		} else {
			set = append(set, edit.KV{Path: ModelPath, Value: selection})
		}
	default:
		return nil, Recovery{}, errors.New("unknown Qoder settings action")
	}
	if len(set) == 0 {
		del = slices.DeleteFunc(del, func(path string) bool { return !config.Get(path).Exists() })
		if len(del) == 0 {
			return raw, recovery, nil
		}
	}
	body, err := edit.PatchJSON(raw, set, del)
	return body, recovery, err
}

func Effort(prefs, model, fallback string) string {
	if level := modelEffort(prefs, model); level != "" {
		return level
	}
	return fallback
}

func modelEffort(prefs, model string) string {
	var models map[string]struct {
		Reasoning  *struct{ Effort string } `json:"reasoning"`
		Generation struct {
			Reasoning *struct{ Effort string } `json:"reasoning"`
		} `json:"generation"`
	}
	if model == "" || json.Unmarshal([]byte(prefs), &models) != nil {
		return ""
	}
	preference := models[model]
	if preference.Reasoning != nil {
		return preference.Reasoning.Effort
	}
	if preference.Generation.Reasoning != nil {
		return preference.Generation.Reasoning.Effort
	}
	return ""
}

func appendEffort(set []edit.KV, prefs, model, effort string) ([]edit.KV, []string) {
	var del []string
	if model != "" {
		next := preferences(prefs, model, effort)
		if len(next) == 0 {
			del = append(del, preferencesPath)
		} else {
			set = append(set, edit.KV{Path: preferencesPath, Value: next})
		}
	}
	if effort == "" {
		del = append(del, fallbackEffortPath)
	} else {
		set = append(set, edit.KV{Path: fallbackEffortPath, Value: effort})
	}
	return set, del
}

func preferences(prefs, model, effort string) map[string]any {
	var out map[string]any
	if err := json.Unmarshal([]byte(prefs), &out); err != nil || out == nil {
		out = map[string]any{}
	}
	preference, _ := out[model].(map[string]any)
	if preference == nil {
		preference = map[string]any{}
	}
	if generation, ok := preference["generation"].(map[string]any); ok {
		delete(generation, "reasoning")
		if len(generation) == 0 {
			delete(preference, "generation")
		}
	}
	reasoning, _ := preference["reasoning"].(map[string]any)
	if reasoning == nil {
		reasoning = map[string]any{}
	}
	if effort == "" {
		delete(reasoning, "effort")
	} else {
		reasoning["effort"] = effort
		if reasoning["enabled"] == false {
			delete(reasoning, "enabled")
		}
	}
	if len(reasoning) == 0 {
		delete(preference, "reasoning")
	} else {
		preference["reasoning"] = reasoning
	}
	if len(preference) == 0 {
		delete(out, model)
	} else {
		out[model] = preference
	}
	return out
}

func keptProvider(current string, want map[string]any) any {
	if name := gjson.Get(current, "displayName").String(); strings.TrimSpace(name) != "" {
		want["displayName"] = name
	}
	var before, after map[string]json.RawMessage
	body, err := json.Marshal(want)
	if err != nil || json.Unmarshal([]byte(current), &before) != nil || json.Unmarshal(body, &after) != nil {
		return want
	}
	return edit.MergeJSONObjects(before, after, []string{"models"})
}
