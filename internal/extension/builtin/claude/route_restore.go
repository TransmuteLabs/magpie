package claude

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

type RouteRestoreOptions struct {
	Claims                       RouteClaims
	Original                     map[string]string
	CloudSwitches                []CloudSwitch
	ModelEnv, TierLabels         []string
	AutoEnv, AutoWritten         string
	AdvisorEnv, AdvisorWritten   string
	OutputWritten, EffortWritten string
	MainWritten                  string
	LocalAliases                 bool
	NextModel                    *string
	Window                       *WindowOptions
}

type CloudSwitch struct {
	Name     string
	Original json.RawMessage
}

type RouteRestoreOwnership struct {
	GatewayRoute      func(url, token string, tokenSet bool) (routed, keyed bool)
	GatewayCredential func(token string) bool
	MagpieModel       func(string) bool
	ReadRoutedModel   func(string) string
	IsRoutedModel     func(string) bool
}

func PlanRouteRestore(options RouteRestoreOptions, settings sdk.ActionSettings, ownership RouteRestoreOwnership) (sdk.AgentPlan, WindowClaims, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != "settings.json" {
		return sdk.AgentPlan{}, WindowClaims{}, errors.New("Claude restore action requires a settings.json snapshot")
	}
	file := settings.Files[0]
	plain := jsonc.ToJSON(edit.TrimBOM(file.Body))
	current := gjson.ParseBytes(plain)
	if len(bytes.TrimSpace(plain)) > 0 && (!json.Valid(plain) || !current.IsObject()) {
		return sdk.AgentPlan{}, WindowClaims{}, edit.AtFile(file.Path, errors.New("invalid Claude settings JSON/JSONC"))
	}
	var set []edit.KV
	var remove []string
	replace := func(path string, value any) {
		if value != nil {
			set = append(set, edit.KV{Path: path, Value: value})
		} else if current.Get(path).Exists() {
			remove = append(remove, path)
		}
	}
	restore := func(path, original string) {
		if original == "" {
			replace(path, nil)
		} else {
			replace(path, original)
		}
	}

	address, token := current.Get(routeURL).String(), current.Get(routeToken).String()
	routed, keyed := ownership.GatewayRoute(address, token, current.Get(routeToken).Exists())
	legacy := options.Claims == nil
	owns := func(path string) bool {
		expected, present := options.Claims[path]
		return present && expected == routeFingerprint(current.Get(path))
	}
	base := address
	if owns(routeURL) || routed || legacy && keyed {
		base = options.Original[routeURL]
		if legacy && !routed {
			base = ""
		}
		restore(routeURL, base)
	}
	if owns(routeToken) || legacy && keyed || ownership.GatewayCredential(token) {
		original := options.Original[routeToken]
		if base != options.Original[routeURL] || legacy && !routed || ownership.GatewayCredential(original) {
			original = ""
		}
		restore(routeToken, original)
	}

	for _, name := range options.ModelEnv {
		path := "env." + name
		value := current.Get(path).String()
		owned := owns(path) || routed && ownership.IsRoutedModel(value) || legacy && (routed || keyed)
		if name == options.AutoEnv {
			owned = value != "" && (value == options.AutoWritten || routed && ownership.IsRoutedModel(value))
			owned = owned || !current.Get(path).Exists() && (routed || options.Original[path] != "")
		}
		if owned {
			original := options.Original[path]
			if ownership.MagpieModel(original) || legacy && !routed {
				original = ""
			}
			restore(path, original)
		} else if base == "" && ownership.MagpieModel(value) {
			replace(path, nil)
		}
	}
	for _, name := range options.TierLabels {
		path := "env." + name
		if original := options.Original[path]; !current.Get(path).Exists() && original != "" {
			restore(path, original)
		}
	}

	model := current.Get("model").String()
	main := ownership.ReadRoutedModel(model)
	if tier := strings.TrimSuffix(model, "[1m]"); options.LocalAliases {
		switch tier {
		case "opus", "sonnet", "haiku", "fable":
			main = ownership.ReadRoutedModel(current.Get("env.ANTHROPIC_DEFAULT_" + strings.ToUpper(tier) + "_MODEL").String())
		}
	}
	legacyModel := legacy && (routed || keyed) && (ownership.MagpieModel(model) ||
		options.MainWritten != "" && main == options.MainWritten || routed && model == "")
	if options.NextModel != nil {
		restore("model", *options.NextModel)
	} else if owns("model") || legacyModel {
		original := options.Original["model"]
		if ownership.MagpieModel(original) || legacy && !routed {
			original = ""
		}
		restore("model", original)
	} else if base == "" && ownership.MagpieModel(model) {
		replace("model", nil)
	}

	removeOwned := func(path, written string) {
		if written != "" && current.Get(path).String() == written {
			replace(path, nil)
		}
	}
	output := "env." + OutputTokensEnv
	if owns(output) || legacy && options.OutputWritten != "" && current.Get(output).String() == options.OutputWritten {
		replace(output, nil)
	}
	removeOwned("env."+EffortEnv, options.EffortWritten)
	removeOwned("env."+options.AdvisorEnv, options.AdvisorWritten)
	if routed && ownership.IsRoutedModel(current.Get("advisorModel").String()) {
		replace("advisorModel", nil)
	}
	for _, cloud := range options.CloudSwitches {
		path := "env." + cloud.Name
		if !current.Get(path).Exists() && json.Valid(cloud.Original) {
			replace(path, cloud.Original)
		}
	}
	windowSet, windowRemove, claims := windowChanges(current, options.Window)
	set, remove = append(set, windowSet...), append(remove, windowRemove...)
	if len(set) == 0 && len(remove) == 0 {
		return sdk.AgentPlan{}, claims, nil
	}
	body, err := edit.PatchJSON(file.Body, set, remove)
	if err != nil {
		return sdk.AgentPlan{}, WindowClaims{}, edit.AtFile(file.Path, err)
	}
	return routeSettingsPlan(file, body), claims, nil
}
