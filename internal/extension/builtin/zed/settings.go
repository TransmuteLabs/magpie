package zed

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
)

const (
	SettingsFile = "settings.json"
	ProviderPath = "language_models.openai_compatible.magpie"
	ModelPath    = "agent.default_model"
	AddressPath  = ProviderPath + ".api_url"
	catalogPath  = ProviderPath + ".available_models"
)

type Action uint8

const (
	SelectModel Action = iota
	RefreshCatalog
	ChangeProxy
)

type Options struct {
	Action   Action
	Value    string
	ModelRef string
	Gateway  string
	Reach    func(address string) string
	Catalog  func() ([]catalog.Model, int)
	Previous map[string]string
}

type Effects struct {
	CredentialURL string
	Save          map[string]string
	Restored      bool
}

func Patch(options Options, raw []byte) ([]byte, Effects, error) {
	config := gjson.ParseBytes(jsonc.ToJSON(edit.TrimBOM(raw)))
	address := config.Get(AddressPath).String()
	var set []edit.KV
	var remove []string
	var effects Effects
	switch options.Action {
	case SelectModel:
		var err error
		set, remove, effects, err = selectModel(options, config, address)
		if err != nil {
			return nil, Effects{}, err
		}
	case RefreshCatalog:
		current := config.Get(catalogPath)
		if !current.Exists() || address == "" || address != options.Reach(address) {
			break
		}
		next := models(current.String(), options.Catalog)
		if !edit.SameJSON(current.Raw, next) {
			set = append(set, edit.KV{Path: catalogPath, Value: next})
		}
	case ChangeProxy:
		if address == "" || address != options.Reach(address) && !strings.HasSuffix(address, options.Gateway) {
			return nil, Effects{}, errors.New("connect Zed to magpie first: pick one of magpie's models for it on the Agents page")
		}
		effects.CredentialURL = options.Value + options.Gateway
		set = append(set, edit.KV{Path: AddressPath, Value: effects.CredentialURL})
	default:
		return nil, Effects{}, errors.New("unknown Zed settings action")
	}
	if len(set) == 0 && len(remove) == 0 {
		return raw, effects, nil
	}
	body, err := edit.PatchJSON(raw, set, remove)
	return body, effects, err
}

func selectModel(options Options, config gjson.Result, address string) ([]edit.KV, []string, Effects, error) {
	current := config.Get(ModelPath)
	model := current.Get("model").String()
	if provider := current.Get("provider").String(); provider != "" && model != "" {
		model = provider + "/" + model
	}
	selected := strings.HasPrefix(model, "magpie/")
	if options.ModelRef != "" {
		reach := options.Reach(address)
		effects := Effects{CredentialURL: reach}
		if !selected {
			effects.Save = map[string]string{"model": current.String()}
			if address == "" || address != reach {
				effects.Save["provider"] = config.Get(ProviderPath).String()
			}
		}
		return []edit.KV{
			{Path: ProviderPath, Value: map[string]any{
				"api_url": reach, "available_models": models(config.Get(catalogPath).String(), options.Catalog),
			}},
			{Path: ModelPath, Value: modelValue("magpie", options.ModelRef)},
		}, nil, effects, nil
	}

	provider, nativeModel, ok := strings.Cut(options.Value, "/")
	if options.Value != "" && (!ok || provider == "" || nativeModel == "") {
		return nil, nil, Effects{}, fmt.Errorf("expected provider/model, got %q", options.Value)
	}
	var set []edit.KV
	var remove []string
	restore := func(name, path string) {
		if value := options.Previous[name]; value != "" {
			set = append(set, edit.KV{Path: path, Value: json.RawMessage(value)})
		} else if config.Get(path).Exists() {
			remove = append(remove, path)
		}
	}
	routed := address != "" && address == options.Reach(address)
	if routed || selected {
		restore("provider", ProviderPath)
		if selected && options.Value == "" {
			restore("model", ModelPath)
		}
	} else if options.Value == "" && current.Exists() {
		remove = append(remove, ModelPath)
	}
	if options.Value != "" {
		set = append(set, edit.KV{Path: ModelPath, Value: modelValue(provider, nativeModel)})
	}
	return set, remove, Effects{Restored: routed || selected}, nil
}

func modelValue(provider, model string) map[string]string {
	return map[string]string{"provider": provider, "model": model}
}

var levels = []string{"minimal", "low", "medium", "high"}

func effort(model catalog.Model) string {
	if !model.Reasoning && len(model.Efforts) == 0 {
		return ""
	}
	best := -1
	for _, value := range model.Efforts {
		index := slices.Index(levels, value)
		if index < 0 && slices.Contains([]string{"xhigh", "max", "ultra"}, value) {
			index = len(levels) - 1
		}
		best = max(best, index)
	}
	if best < 0 {
		return "high"
	}
	return levels[best]
}

func models(raw string, catalog func() ([]catalog.Model, int)) []any {
	kept := map[string]json.RawMessage{}
	var current []struct {
		Name   string          `json:"name"`
		Effort json.RawMessage `json:"reasoning_effort"`
	}
	if raw != "" && json.Unmarshal([]byte(raw), &current) == nil {
		for _, entry := range current {
			if len(entry.Effort) > 0 && string(entry.Effort) != "null" {
				kept[entry.Name] = entry.Effort
			}
		}
	}
	available, fallbackContext := catalog()
	models := make([]any, 0, len(available))
	for _, model := range available {
		context := model.Context
		if context == 0 {
			context = fallbackContext
		}
		entry := map[string]any{
			"name": model.ID, "display_name": model.Name, "max_tokens": context,
			"capabilities": map[string]any{
				"tools": true, "images": model.Images,
				"parallel_tool_calls": false, "prompt_cache_key": false,
				"chat_completions": true,
			},
		}
		if output := model.Output; output > 0 {
			if model.Context == 0 {
				output = min(output, context)
			}
			entry["max_output_tokens"] = output
			entry["max_tokens"] = context + output
		}
		if value, ok := kept[model.ID]; ok {
			entry["reasoning_effort"] = value
		} else if value := effort(model); value != "" {
			entry["reasoning_effort"] = value
		}
		models = append(models, entry)
	}
	return models
}
