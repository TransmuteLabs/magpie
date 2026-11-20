package gemini

import (
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

func PlanRoute(options RouteOptions, settings sdk.ActionSettings) (sdk.AgentPlan, *OriginalSettings, error) {
	files := settings.Files
	if len(files) != 2 || files[0].Path != EnvFile || files[1].Path != SettingsFile {
		return sdk.AgentPlan{}, nil, errors.New("Gemini CLI route needs dotenv and JSON settings")
	}
	env, config := files[0], files[1]
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
