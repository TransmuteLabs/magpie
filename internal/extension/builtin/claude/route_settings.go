package claude

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const OutputTokensEnv = "CLAUDE_CODE_MAX_OUTPUT_TOKENS"

type RouteModels struct {
	Main, Opus, Sonnet, Haiku, Fable string
	LocalAliases                     bool
}

// MainWord uses a native tier alias on the local settings file when a tier
// carries the main model. Desktop's Code tab reads this file but may drop
// the environment and send the model directly to Anthropic.
func (models RouteModels) MainWord() string {
	if models.LocalAliases {
		for _, tier := range []struct{ name, model string }{
			{"sonnet", models.Sonnet}, {"opus", models.Opus},
			{"haiku", models.Haiku}, {"fable", models.Fable},
		} {
			if tier.model != "" && tier.model == models.Main {
				if strings.HasSuffix(models.Main, "[1m]") {
					return tier.name + "[1m]"
				}
				return tier.name
			}
		}
	}
	return models.Main
}

type RouteSettingsOptions struct {
	GatewayURL, GatewayToken string
	Models                   RouteModels
	Subagent                 string
	OutputTokens             int
	PreviousOutput           string
	Previous                 RouteClaims
}

type RouteClaims map[string]string

type RouteSettingsResult struct {
	Output string
	Claims RouteClaims
}

const (
	routeURL   = "env.ANTHROPIC_BASE_URL"
	routeToken = "env.ANTHROPIC_AUTH_TOKEN"
)

func routeFingerprint(value gjson.Result) string {
	if !value.Exists() {
		return ""
	}
	sum := sha256.Sum256([]byte(value.Raw))
	return hex.EncodeToString(sum[:])
}

func PlanRouteSettings(options RouteSettingsOptions, settings sdk.ActionSettings, keepCallerKey func(string) bool) (sdk.AgentPlan, RouteSettingsResult, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != "settings.json" {
		return sdk.AgentPlan{}, RouteSettingsResult{}, errors.New("Claude route settings action requires a settings.json snapshot")
	}
	file := settings.Files[0]
	before := gjson.ParseBytes(jsonc.ToJSON(edit.TrimBOM(file.Body)))
	token := options.GatewayToken
	if current := before.Get(routeToken).String(); current != token && keepCallerKey(current) {
		token = current
	}
	set := []edit.KV{
		{Path: routeURL, Value: options.GatewayURL},
		{Path: routeToken, Value: token},
		{Path: "env.ANTHROPIC_SMALL_FAST_MODEL", Value: options.Models.Haiku},
		{Path: "model", Value: options.Models.MainWord()},
	}
	remove := []string{"env.ANTHROPIC_MODEL"}
	for _, binding := range []struct{ key, model string }{
		{"env.ANTHROPIC_DEFAULT_OPUS_MODEL", options.Models.Opus},
		{"env.ANTHROPIC_DEFAULT_SONNET_MODEL", options.Models.Sonnet},
		{"env.ANTHROPIC_DEFAULT_HAIKU_MODEL", options.Models.Haiku},
		{"env.ANTHROPIC_DEFAULT_FABLE_MODEL", options.Models.Fable},
		{"env.CLAUDE_CODE_SUBAGENT_MODEL", options.Subagent},
	} {
		if binding.model == "" {
			remove = append(remove, binding.key)
		} else {
			set = append(set, edit.KV{Path: binding.key, Value: binding.model})
		}
	}
	outputKey := "env." + OutputTokensEnv
	current := before.Get(outputKey).String()
	owned := current != "" && current == options.PreviousOutput
	var output string
	if current == "" || owned {
		if options.OutputTokens > 0 {
			output = strconv.Itoa(options.OutputTokens)
			set = append(set, edit.KV{Path: outputKey, Value: output})
		} else if owned {
			remove = append(remove, outputKey)
		}
	}
	body, err := edit.PatchJSON(file.Body, nil, remove)
	if err == nil {
		body, err = edit.PatchJSON(body, set, nil)
	}
	if err != nil {
		return sdk.AgentPlan{}, RouteSettingsResult{}, edit.AtFile(file.Path, err)
	}
	after := gjson.ParseBytes(jsonc.ToJSON(edit.TrimBOM(body)))
	claims := make(RouteClaims, len(set)+len(remove))
	record := func(path string) {
		fingerprint := routeFingerprint(after.Get(path))
		old, owned := options.Previous[path]
		if options.Previous == nil || fingerprint != routeFingerprint(before.Get(path)) || owned && old == fingerprint {
			claims[path] = fingerprint
		}
	}
	for _, change := range set {
		if change.Path != routeToken || token == options.GatewayToken {
			record(change.Path)
		}
	}
	for _, path := range remove {
		record(path)
	}
	return routeSettingsPlan(file, body), RouteSettingsResult{Output: output, Claims: claims}, nil
}

func PlanRouteLogin(token string, settings sdk.ActionSettings) (sdk.AgentPlan, string, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != "settings.json" {
		return sdk.AgentPlan{}, "", errors.New("Claude sign-in action requires a settings.json snapshot")
	}
	file := settings.Files[0]
	body, err := edit.PatchJSON(file.Body, []edit.KV{{Path: routeToken, Value: token}}, nil)
	if err != nil {
		return sdk.AgentPlan{}, "", edit.AtFile(file.Path, err)
	}
	plain := jsonc.ToJSON(edit.TrimBOM(body))
	return routeSettingsPlan(file, body), routeFingerprint(gjson.GetBytes(plain, routeToken)), nil
}

func routeSettingsPlan(file sdk.ActionSettingFile, body []byte) sdk.AgentPlan {
	if bytes.Equal(file.Body, body) {
		return sdk.AgentPlan{}
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: file.Path, BeforeHash: file.BeforeHash, Body: body}}}
}
