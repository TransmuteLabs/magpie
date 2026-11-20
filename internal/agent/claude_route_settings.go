package agent

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/agentenv"
	"github.com/yetone/magpie/internal/extension"
	builtinclaude "github.com/yetone/magpie/internal/extension/builtin/claude"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func claudeRouteClaims(saved string) (builtinclaude.RouteClaims, error) {
	if saved == "" {
		return nil, nil
	}
	var claims builtinclaude.RouteClaims
	if err := json.Unmarshal([]byte(saved), &claims); err != nil || claims == nil {
		return nil, errors.New("invalid saved Claude Code route ownership")
	}
	return claims, nil
}

func applyClaudeRouteSettings(path string, options builtinclaude.RouteSettingsOptions) (builtinclaude.RouteSettingsResult, error) {
	var result builtinclaude.RouteSettingsResult
	err := extension.ApplyLinkedAgentPlan(options, func(options builtinclaude.RouteSettingsOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
		plan, next, err := builtinclaude.PlanRouteSettings(options, settings, func(key string) bool {
			return keyHolds(key, options.GatewayToken) && !access.Default(key)
		})
		result = next
		return plan, err
	}, extension.LinkedActionFile{Name: "settings.json", Path: path})
	return result, err
}

func claudeRouteEnvOptions(at place, saved map[string]string, tiers map[string]string) builtinclaude.RouteEnvOptions {
	return builtinclaude.RouteEnvOptions{
		Tiers: claudeTiers, FirstTierPass: saved[at.key("claude.tier_about")] == "", NextTiers: tiers,
		MatchesTierModel: func(current, next string) bool {
			return claudeSameModel(claudeReadOwn(current), next)
		},
	}
}

func claudeRouteEnvUpdates(at place, saved map[string]string, result builtinclaude.RouteEnvResult) map[string]string {
	updates := map[string]string{}
	for _, cloud := range result.CloudSwitches {
		updates[at.key("claude.cloud."+cloud.Name)] = string(cloud.Original)
	}
	for name, value := range result.OriginalLabels {
		key := at.key("claude.env." + name)
		if result.ReplaceOriginalLabels || saved[key] == "" {
			updates[key] = value
		}
	}
	if result.CheckedLabels {
		updates[at.key("claude.tier_about")] = "1"
	}
	return updates
}

func repairClaudeRouteEnv(at place, path string) error {
	saved := stashShared()
	var result builtinclaude.RouteEnvResult
	err := extension.ApplyLinkedAgentPlan(claudeRouteEnvOptions(at, saved, nil), func(options builtinclaude.RouteEnvOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
		plan, cleanup, err := builtinclaude.PlanRouteEnv(options, settings)
		result = cleanup
		return plan, err
	}, extension.LinkedActionFile{Name: "settings.json", Path: path})
	if err == nil {
		if updates := claudeRouteEnvUpdates(at, saved, result); len(updates) > 0 {
			stash(updates)
		}
	}
	return err
}

func applyClaudeRouteLogin(path, token string) (string, error) {
	var fingerprint string
	err := extension.ApplyLinkedAgentPlan(token, func(token string, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
		plan, claim, err := builtinclaude.PlanRouteLogin(token, settings)
		fingerprint = claim
		return plan, err
	}, extension.LinkedActionFile{Name: "settings.json", Path: path})
	return fingerprint, err
}

func restoreClaudeRoute(at place, path string, nextModel *string, effort string) error {
	metadata := []string{"claude" + wireClaims, "claude.main", "claude" + wiredAt, "claude.tier_about",
		"claude.auto_mode_model", "claude.advisor_env", "claude.output_tokens"}
	originals := map[string]string{
		"model": "claude.model", "env.ANTHROPIC_BASE_URL": "claude.base_url", "env.ANTHROPIC_AUTH_TOKEN": "claude.auth_token",
	}
	for _, name := range claudeOwnEnv {
		originals["env."+name] = "claude.env." + name
	}
	keys := make([]string, 0, len(metadata)+len(originals)+len(agentenv.ClaudeProviders)+1)
	for _, key := range metadata {
		keys = append(keys, at.key(key))
	}
	for _, key := range originals {
		keys = append(keys, at.key(key))
	}
	for _, name := range agentenv.ClaudeProviders {
		keys = append(keys, at.key("claude.cloud."+name))
	}
	login := at.key("claude.login")
	keys = append(keys, login)
	saved := peek(keys...)
	value := func(key string) string { return saved[at.key(key)] }
	claims, err := claudeRouteClaims(value("claude" + wireClaims))
	if err != nil {
		return err
	}
	options := builtinclaude.RouteRestoreOptions{
		Claims: claims, Original: make(map[string]string, len(originals)),
		ModelEnv: claudeModelEnv, TierLabels: claudeAboutEnv, NextModel: nextModel,
		AutoEnv: claudeAutoModeEnv, AutoWritten: value("claude.auto_mode_model"),
		AdvisorEnv: claudeAdvisorEnv, AdvisorWritten: value("claude.advisor_env"),
		OutputWritten: value("claude.output_tokens"), EffortWritten: effort,
		MainWritten: strings.TrimSuffix(value("claude.main"), "[1m]"), LocalAliases: at.id == "",
	}
	for path, key := range originals {
		options.Original[path] = value(key)
	}
	for _, name := range agentenv.ClaudeProviders {
		if original := value("claude.cloud." + name); original != "" {
			options.CloudSwitches = append(options.CloudSwitches, builtinclaude.CloudSwitch{Name: name, Original: json.RawMessage(original)})
		}
	}
	gw, gatewayKey := at.gw(), at.gwKey()
	writtenURL := value("claude" + wiredAt)
	keepsLogin := saved[login] == "claudeai"
	readRoutedModel := func(value string) string {
		model, _ := tierAt(claudeReadOwn(value))
		return strings.TrimSuffix(model, "[1m]")
	}
	ownership := builtinclaude.RouteRestoreOwnership{
		GatewayRoute: func(url, token string, tokenSet bool) (bool, bool) {
			keyed := ourKey(token) || tokenSet && token == "" && keepsLogin && url != "" && url == writtenURL
			accepted := token != "" && keyHolds(token, gatewayKey) || token == "" && keepsLogin && writtenURL != ""
			return url == gw || accepted && surelyMagpie(url, gw), keyed
		},
		GatewayCredential: func(token string) bool {
			return token != "" && (ourKey(token) || keyHolds(token, gatewayKey))
		},
		MagpieModel:     claudeMagpieModel,
		ReadRoutedModel: readRoutedModel,
		IsRoutedModel:   func(value string) bool { return isMagpie(readRoutedModel(value)) },
	}
	err = extension.ApplyLinkedAgentPlan(options, func(options builtinclaude.RouteRestoreOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
		return builtinclaude.PlanRouteRestore(options, settings, ownership)
	}, extension.LinkedActionFile{Name: "settings.json", Path: path})
	if err == nil {
		delete(saved, login)
		forgetValues(saved)
	}
	return err
}
