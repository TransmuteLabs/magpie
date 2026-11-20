package opencode

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	ConfigFile     = "config.json"
	TitleModelKey  = "agents.title.model"
	NativeProvider = "providers.magpie"
	legacyProvider = "provider.magpie"
	smallModelKey  = "small_model"
)

type V2ConfigOptions struct {
	ConvertProvider func([]byte) (json.Marshaler, bool)
	DiscoverModels  func(gjson.Result, string) (json.Marshaler, bool)
	RefreshProvider func(config gjson.Result, put bool) any
	PutProvider     bool
	Primary         *PrimaryModelOptions
}

type PrimaryModelOptions struct {
	Model        string
	Routed       bool
	OwnModel     func(gjson.Result) string
	KeepProvider func(gjson.Result) bool
}

type SmallModelOptions struct {
	Two   bool
	Model string
}

func PlanV2Config(options V2ConfigOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	return planConfig(settings, func(body []byte) ([]byte, error) {
		config := readConfig(body)
		var set []edit.KV
		put := options.PutProvider
		if primary := options.Primary; primary != nil {
			if primary.Model == "" {
				remove := []string{"model"}
				if !primary.KeepProvider(config) {
					remove = append(remove, legacyProvider, NativeProvider)
				}
				for _, key := range remove {
					if config.Get(key).Exists() {
						return edit.PatchJSON(body, nil, remove)
					}
				}
				return body, nil
			}
			model := primary.Model
			if primary.Routed {
				if own := primary.OwnModel(config); own != "" {
					model = own
				} else {
					put = true
				}
			}
			set = append(set, edit.KV{Path: "model", Value: model})
			if !put {
				return edit.PatchJSON(body, set, nil)
			}
		}
		body, err := normalizeV2(body, config, options.ConvertProvider)
		if err != nil {
			return nil, err
		}
		config = readConfig(body)
		const models = NativeProvider + ".models"
		if options.DiscoverModels != nil {
			if left, changed := options.DiscoverModels(config, config.Get(models).Raw); changed {
				if left == nil {
					body, err = edit.PatchJSON(body, nil, []string{models})
				} else {
					body, err = edit.PatchJSON(body, []edit.KV{{Path: models, Value: left}}, nil)
				}
				if err != nil {
					return nil, err
				}
				config = readConfig(body)
			}
		}
		if provider := options.RefreshProvider(config, put); provider != nil {
			set = append(set, edit.KV{Path: NativeProvider, Value: provider})
		}
		if len(set) == 0 {
			return body, nil
		}
		return edit.PatchJSON(body, set, nil)
	})
}

func normalizeV2(body []byte, config gjson.Result, convert func([]byte) (json.Marshaler, bool)) ([]byte, error) {
	var set []edit.KV
	var remove []string
	legacy := config.Get(legacyProvider)
	removeProvider := false
	if legacy.Exists() {
		if config.Get(NativeProvider).Exists() {
			removeProvider = true
		} else if provider, ok := convert([]byte(legacy.Raw)); ok {
			set = append(set, edit.KV{Path: NativeProvider, Value: provider})
			removeProvider = true
		}
		if removeProvider {
			remove = append(remove, legacyProvider)
		}
	}
	if small := config.Get(smallModelKey).String(); strings.HasPrefix(small, "magpie/") && !config.Get(TitleModelKey).Exists() {
		set = append(set, edit.KV{Path: TitleModelKey, Value: small})
		remove = append(remove, smallModelKey)
	}
	if len(set) == 0 && len(remove) == 0 {
		return body, nil
	}
	body, err := edit.PatchJSON(body, set, remove)
	if err != nil || !removeProvider {
		return body, err
	}
	return dropEmpty(body, "provider")
}

func PlanSmallModel(options SmallModelOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	return planConfig(settings, func(body []byte) ([]byte, error) {
		if options.Model == "" && len(bytes.TrimSpace(edit.TrimBOM(body))) == 0 {
			return body, nil
		}
		if !options.Two {
			if options.Model == "" {
				return edit.PatchJSON(body, nil, []string{smallModelKey})
			}
			return edit.PatchJSON(body, []edit.KV{{Path: smallModelKey, Value: options.Model}}, nil)
		}
		if options.Model != "" {
			return edit.PatchJSON(body, []edit.KV{{Path: TitleModelKey, Value: options.Model}}, []string{smallModelKey})
		}
		body, err := edit.PatchJSON(body, nil, []string{smallModelKey, TitleModelKey})
		if err != nil {
			return nil, err
		}
		return dropEmpty(body, "agents.title", "agents")
	})
}

func planConfig(settings sdk.ActionSettings, rewrite func([]byte) ([]byte, error)) (sdk.AgentPlan, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != ConfigFile {
		return sdk.AgentPlan{}, errors.New("OpenCode settings action requires a config.json snapshot")
	}
	config := settings.Files[0]
	body, err := rewrite(config.Body)
	if err != nil {
		return sdk.AgentPlan{}, edit.AtFile(config.Path, err)
	}
	if bytes.Equal(body, config.Body) {
		return sdk.AgentPlan{}, nil
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: config.Path, BeforeHash: config.BeforeHash, Body: body}}}, nil
}

func readConfig(body []byte) gjson.Result {
	return gjson.ParseBytes(jsonc.ToJSON(edit.TrimBOM(body)))
}

func dropEmpty(body []byte, keys ...string) ([]byte, error) {
	for _, key := range keys {
		obj := readConfig(body).Get(key)
		if obj.IsObject() && len(obj.Map()) == 0 {
			var err error
			body, err = edit.PatchJSON(body, nil, []string{key})
			if err != nil {
				return nil, err
			}
		}
	}
	return body, nil
}
