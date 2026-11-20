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
	Gateway     string
	OwnKey      func(string) bool
	Original    *OriginalSettings
	NativeModel string
	NativeAuth  AuthChoice
	ProviderKey string
}

type AuthChoice string

const (
	AuthDefault AuthChoice = "default"
	AuthCustom  AuthChoice = "custom"
	AuthGoogle  AuthChoice = "google"
	AuthVertex  AuthChoice = "vertex"
	AuthAPIKey  AuthChoice = "api-key"
)

var ErrCustomEndpoint = errors.New("no saved custom Gemini endpoint")

func PlanRestore(options RestoreOptions, settings sdk.ActionSettings) (sdk.AgentPlan, bool, error) {
	var plan sdk.AgentPlan
	env, config, err := settingsPair(settings)
	if err != nil {
		return plan, false, err
	}
	base, _ := edit.GetEnv(env.Body, baseKey)
	key, _ := edit.GetEnv(env.Body, apiKey)
	routed := Routed(base, key, options.Gateway, options.OwnKey)
	if !routed && options.Original != nil && options.NativeModel == "" && options.NativeAuth == "" {
		return plan, false, nil
	}
	var putJSON []edit.KV
	var delJSON []string
	put := func(path, value string) {
		if value == "" {
			delJSON = append(delJSON, path)
		} else {
			putJSON = append(putJSON, edit.KV{Path: path, Value: value})
		}
	}
	var original OriginalSettings
	if options.Original != nil {
		original = *options.Original
	}
	nextBase, nextKey := base, key
	if routed {
		nextBase, nextKey = original.Base, original.Key
	}
	nextAuth := original.Auth
	switch options.NativeAuth {
	case "":
	case AuthCustom:
		if nextBase == "" || Routed(nextBase, nextKey, options.Gateway, options.OwnKey) {
			return plan, false, ErrCustomEndpoint
		}
	case AuthDefault:
		nextBase, nextAuth = "", ""
	case AuthGoogle:
		nextBase, nextAuth = "", "oauth-personal"
	case AuthVertex:
		nextBase, nextAuth = "", "vertex-ai"
	case AuthAPIKey:
		nextBase, nextAuth = "", "gemini-api-key"
		if options.ProviderKey != "" {
			nextKey = options.ProviderKey
		}
	default:
		return plan, false, errors.New("unknown Gemini CLI auth choice")
	}
	if routed || options.NativeAuth != "" && options.NativeAuth != AuthCustom {
		put(authKey, nextAuth)
	}
	if options.NativeModel != "" {
		put(modelKey, options.NativeModel)
	} else if routed || options.Original == nil {
		put(modelKey, original.Model)
	}
	envBody := env.Body
	var putEnv []edit.KV
	if routed {
		envBody, _ = edit.PatchEnvWithout(envBody, baseKey, apiKey)
		if nextBase != "" {
			putEnv = append(putEnv, edit.KV{Path: baseKey, Value: nextBase})
		}
		if nextKey != "" {
			putEnv = append(putEnv, edit.KV{Path: apiKey, Value: nextKey})
		}
	} else {
		if options.NativeAuth != "" && options.NativeAuth != AuthCustom {
			envBody, _ = edit.PatchEnvWithout(envBody, baseKey)
		}
		if nextKey != key {
			putEnv = append(putEnv, edit.KV{Path: apiKey, Value: nextKey})
		}
	}
	if len(putEnv) > 0 {
		envBody = edit.PatchEnv(envBody, putEnv...)
	}
	if !bytes.Equal(env.Body, envBody) {
		plan.Files = append(plan.Files, sdk.FileChange{Path: env.Path, BeforeHash: env.BeforeHash, Body: envBody, Remove: envBody == nil})
	}
	if len(putJSON) == 0 && len(delJSON) == 0 {
		return plan, routed, nil
	}
	if len(bytes.TrimSpace(edit.TrimBOM(config.Body))) == 0 && len(putJSON) == 0 {
		return plan, routed, nil
	}
	body, err := edit.PatchJSON(config.Body, putJSON, delJSON)
	if err == nil && (routed || options.Original == nil) {
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
