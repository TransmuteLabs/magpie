package gemini

import (
	"bytes"
	"errors"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	EnvFile      = ".env"
	SettingsFile = "settings.json"
	baseKey      = "GOOGLE_GEMINI_BASE_URL"
	apiKey       = "GEMINI_API_KEY"
	authKey      = "security.auth.selectedType"
	modelKey     = "model.name"
)

type RouteOptions struct {
	Gateway string
	Key     string
	Model   string
	OwnKey  func(string) bool
}

type OriginalSettings struct {
	Base  string
	Key   string
	Auth  string
	Model string
}

func Routed(base, key, gateway string, ownKey func(string) bool) bool {
	return base == gateway || base != "" && ownKey(key)
}

func settingsPair(settings sdk.ActionSettings) (env, config sdk.ActionSettingFile, err error) {
	files := settings.Files
	if len(files) != 2 || files[0].Path != EnvFile || files[1].Path != SettingsFile {
		return env, config, errors.New("Gemini CLI settings plan needs dotenv and JSON settings")
	}
	return files[0], files[1], nil
}

func PlanRoute(options RouteOptions, settings sdk.ActionSettings) (sdk.AgentPlan, *OriginalSettings, error) {
	env, config, err := settingsPair(settings)
	if err != nil {
		return sdk.AgentPlan{}, nil, err
	}
	body, err := edit.PatchJSON(config.Body, []edit.KV{
		{Path: authKey, Value: "gemini-api-key"},
		{Path: modelKey, Value: options.Model},
	}, nil)
	if err != nil {
		return sdk.AgentPlan{}, nil, edit.AtFile(config.Path, err)
	}
	base, _ := edit.GetEnv(env.Body, baseKey)
	key, _ := edit.GetEnv(env.Body, apiKey)
	var original *OriginalSettings
	if !Routed(base, key, options.Gateway, options.OwnKey) {
		prior := gjson.ParseBytes(jsonc.ToJSON(edit.TrimBOM(config.Body)))
		original = &OriginalSettings{Base: base, Key: key, Auth: prior.Get(authKey).String(), Model: prior.Get(modelKey).String()}
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{
		{Path: env.Path, BeforeHash: env.BeforeHash, Body: edit.PatchEnv(env.Body, edit.KV{Path: baseKey, Value: options.Gateway}, edit.KV{Path: apiKey, Value: options.Key})},
		{Path: config.Path, BeforeHash: config.BeforeHash, Body: body},
	}}, original, nil
}

type RestoreOptions struct {
	Gateway  string
	OwnKey   func(string) bool
	Original *OriginalSettings
}

func PlanRestore(options RestoreOptions, settings sdk.ActionSettings) (sdk.AgentPlan, bool, error) {
	var plan sdk.AgentPlan
	env, config, err := settingsPair(settings)
	if err != nil {
		return plan, false, err
	}
	base, _ := edit.GetEnv(env.Body, baseKey)
	key, _ := edit.GetEnv(env.Body, apiKey)
	routed := Routed(base, key, options.Gateway, options.OwnKey)
	if !routed && options.Original != nil {
		return plan, false, nil
	}
	var putJSON, putEnv []edit.KV
	var delJSON []string
	put := func(path, value string) {
		if value == "" {
			delJSON = append(delJSON, path)
		} else {
			putJSON = append(putJSON, edit.KV{Path: path, Value: value})
		}
	}
	if routed {
		var original OriginalSettings
		if options.Original != nil {
			original = *options.Original
		}
		put(authKey, original.Auth)
		put(modelKey, original.Model)
		if original.Base != "" {
			putEnv = append(putEnv, edit.KV{Path: baseKey, Value: original.Base})
		}
		if original.Key != "" {
			putEnv = append(putEnv, edit.KV{Path: apiKey, Value: original.Key})
		}
		body, changed := edit.PatchEnvWithout(env.Body, baseKey, apiKey)
		if len(putEnv) > 0 {
			body, changed = edit.PatchEnv(body, putEnv...), true
		}
		if changed {
			plan.Files = append(plan.Files, sdk.FileChange{Path: env.Path, BeforeHash: env.BeforeHash, Body: body, Remove: body == nil})
		}
	} else {
		put(modelKey, "")
	}
	if len(bytes.TrimSpace(edit.TrimBOM(config.Body))) == 0 && len(putJSON) == 0 {
		return plan, routed, nil
	}
	body, err := edit.PatchJSON(config.Body, putJSON, delJSON)
	if err == nil {
		model := gjson.GetBytes(jsonc.ToJSON(edit.TrimBOM(body)), "model")
		if model.IsObject() && len(model.Map()) == 0 {
			body, err = edit.PatchJSON(body, nil, []string{"model"})
		}
	}
	if err != nil {
		return sdk.AgentPlan{}, false, edit.AtFile(config.Path, err)
	}
	if !bytes.Equal(config.Body, body) {
		plan.Files = append(plan.Files, sdk.FileChange{Path: config.Path, BeforeHash: config.BeforeHash, Body: body})
	}
	return plan, routed, nil
}
