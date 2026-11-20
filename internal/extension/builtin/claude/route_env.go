package claude

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/agentenv"
	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

var tierLabelSuffixes = []string{"_NAME", "_DESCRIPTION", "_SUPPORTED_CAPABILITIES"}

func TierModelEnv(tier string) string {
	return "ANTHROPIC_DEFAULT_" + strings.ToUpper(tier) + "_MODEL"
}

func TierLabelKeys(tiers []string) []string {
	keys := make([]string, 0, len(tiers)*len(tierLabelSuffixes))
	for _, tier := range tiers {
		for _, suffix := range tierLabelSuffixes {
			keys = append(keys, TierModelEnv(tier)+suffix)
		}
	}
	return keys
}

func CloudSwitchOn(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

type RouteEnvOptions struct {
	Tiers                 []string
	FirstTierPass         bool
	ReplaceOriginalLabels bool
	NextTiers             map[string]string
	MatchesTierModel      func(current, next string) bool
	AdoptClassifier       func(current string) (original string, remove bool)
}

type RouteEnvResult struct {
	CloudSwitches         []CloudSwitch
	OriginalLabels        map[string]string
	CheckedLabels         bool
	ReplaceOriginalLabels bool
	OriginalClassifier    *string
}

func PlanRouteEnv(options RouteEnvOptions, settings sdk.ActionSettings) (sdk.AgentPlan, RouteEnvResult, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != "settings.json" {
		return sdk.AgentPlan{}, RouteEnvResult{}, errors.New("Claude route environment action requires a settings.json snapshot")
	}
	file := settings.Files[0]
	current := gjson.ParseBytes(jsonc.ToJSON(edit.TrimBOM(file.Body)))
	remove, result := routeEnvChanges(options, current)
	if len(remove) == 0 {
		return sdk.AgentPlan{}, result, nil
	}
	body, err := edit.PatchJSON(file.Body, nil, remove)
	if err != nil {
		return sdk.AgentPlan{}, RouteEnvResult{}, edit.AtFile(file.Path, err)
	}
	return routeSettingsPlan(file, body), result, nil
}

func routeEnvChanges(options RouteEnvOptions, current gjson.Result) ([]string, RouteEnvResult) {
	var remove []string
	result := RouteEnvResult{
		CheckedLabels: options.FirstTierPass, ReplaceOriginalLabels: options.ReplaceOriginalLabels,
	}
	if options.AdoptClassifier != nil {
		const path = "env." + AutoModeEnv
		original, drop := options.AdoptClassifier(current.Get(path).String())
		result.OriginalClassifier = &original
		if drop {
			remove = append(remove, path)
		}
	}
	for _, name := range agentenv.ClaudeProviders {
		path := "env." + name
		value := current.Get(path)
		if CloudSwitchOn(value.String()) {
			remove = append(remove, path)
			result.CloudSwitches = append(result.CloudSwitches, CloudSwitch{Name: name, Original: json.RawMessage(value.Raw)})
		}
	}
	for _, tier := range options.Tiers {
		model := TierModelEnv(tier)
		stays := options.NextTiers == nil
		if !stays {
			existing, next := current.Get("env."+model).String(), options.NextTiers[tier]
			stays = existing == next
			if options.MatchesTierModel != nil {
				stays = options.MatchesTierModel(existing, next)
			}
		}
		removeLabels := options.FirstTierPass || !stays
		if !removeLabels && !options.ReplaceOriginalLabels {
			continue
		}
		for _, suffix := range tierLabelSuffixes {
			name := model + suffix
			path := "env." + name
			label := current.Get(path)
			if removeLabels && label.Exists() {
				remove = append(remove, path)
			}
			if value := label.String(); options.ReplaceOriginalLabels || options.FirstTierPass && value != "" {
				if result.OriginalLabels == nil {
					result.OriginalLabels = map[string]string{}
				}
				result.OriginalLabels[name] = value
			}
		}
	}
	return remove, result
}
