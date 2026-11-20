package claude

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	ContextEnv = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"
	CompactEnv = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
)

type WindowClaims struct {
	Context, Default, Compact string
	Budgets                   map[string]int
	Long                      bool
}

type WindowState struct {
	UserContext, UserCompact, UserDefault bool
	Default                               string
	UserCompactSetting                    bool
	UserBudgets                           map[string]string
}

type WindowChoice struct {
	Context, Default, Compact int
	Budgets                   map[string]int
	Long                      bool
}

type WindowOptions struct {
	Previous      WindowClaims
	ManageCompact bool
	MatchesModel  func(string) bool
	Choose        func(WindowState) WindowChoice
}

type ModelSettingsOptions struct {
	Window  *WindowOptions
	Catalog *CatalogOptions
}

type ModelSettingsClaims struct {
	Window  WindowClaims
	Catalog CatalogClaims
}

func modelSettingsChanges(current gjson.Result, options ModelSettingsOptions) ([]edit.KV, []string, ModelSettingsClaims, error) {
	set, remove, window := windowChanges(current, options.Window)
	catalogSet, catalogRemove, catalog, err := catalogChanges(current, options.Catalog)
	return append(set, catalogSet...), append(remove, catalogRemove...), ModelSettingsClaims{Window: window, Catalog: catalog}, err
}

func PlanModelSettings(options ModelSettingsOptions, settings sdk.ActionSettings) (sdk.AgentPlan, ModelSettingsClaims, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != "settings.json" {
		return sdk.AgentPlan{}, ModelSettingsClaims{}, errors.New("Claude model settings action requires a settings.json snapshot")
	}
	file := settings.Files[0]
	current := gjson.ParseBytes(jsonc.ToJSON(edit.TrimBOM(file.Body)))
	set, remove, claims, err := modelSettingsChanges(current, options)
	if err != nil {
		return sdk.AgentPlan{}, ModelSettingsClaims{}, edit.AtFile(file.Path, err)
	}
	if len(set) == 0 && len(remove) == 0 {
		return sdk.AgentPlan{}, claims, nil
	}
	body, err := edit.PatchJSON(file.Body, set, remove)
	if err != nil {
		return sdk.AgentPlan{}, ModelSettingsClaims{}, edit.AtFile(file.Path, err)
	}
	return routeSettingsPlan(file, body), claims, nil
}

func BudgetKey(id string) string { return strings.ToLower(strings.TrimSuffix(id, "[1m]")) }

func BudgetKeyOK(id string) bool { return id != "" && !strings.ContainsAny(id, `*?|#@\"`) }

func budgetObject(id string) string {
	return "modelSettings." + strings.ReplaceAll(strings.TrimSuffix(id, "[1m]"), ".", `\.`)
}

func BudgetPath(id string) string { return budgetObject(id) + ".autoCompactWindow" }

func readBudgets(raw string) map[string]map[string]json.RawMessage {
	var per map[string]map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &per) != nil {
		return nil
	}
	return per
}

func budgetValue(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}

func windowChanges(current gjson.Result, options *WindowOptions) ([]edit.KV, []string, WindowClaims) {
	if options == nil {
		return nil, nil, WindowClaims{}
	}
	if options.Choose == nil {
		return nil, nil, options.Previous
	}
	const contextPath, compactPath, defaultPath = "env." + ContextEnv, "env." + CompactEnv, "autoCompactWindow"
	context, compact, top := current.Get(contextPath), current.Get(compactPath), current.Get(defaultPath)
	previous := options.Previous
	owned := func(value gjson.Result, written string) bool { return written != "" && value.String() == written }
	state := WindowState{
		UserContext: context.String() != "" && !owned(context, previous.Context),
		UserCompact: compact.String() != "" && !owned(compact, previous.Compact),
		UserDefault: top.Exists() && !owned(top, previous.Default),
		Default:     top.String(),
		UserBudgets: map[string]string{},
	}
	state.UserCompactSetting = state.UserDefault
	per := readBudgets(current.Get("modelSettings").Raw)
	for id, fields := range per {
		value, present := fields[defaultPath]
		if !present {
			continue
		}
		text := budgetValue(value)
		if number, ok := previous.Budgets[id]; ok && text == strconv.Itoa(number) {
			continue
		}
		state.UserBudgets[BudgetKey(id)] = text
		if string(value) != "null" && options.MatchesModel != nil && options.MatchesModel(id) {
			state.UserCompactSetting = true
		}
	}
	choice := options.Choose(state)
	var set []edit.KV
	var remove []string
	var claims WindowClaims
	write := func(path string, current gjson.Result, previous string, next int, user bool, asString bool) string {
		if user {
			return ""
		}
		if next == 0 {
			if owned(current, previous) {
				remove = append(remove, path)
			}
			return ""
		}
		text := strconv.Itoa(next)
		if current.String() != text {
			var value any = next
			if asString {
				value = text
			}
			set = append(set, edit.KV{Path: path, Value: value})
		}
		return text
	}
	claims.Context = write(contextPath, context, previous.Context, choice.Context, state.UserContext, true)
	claims.Default = write(defaultPath, top, previous.Default, choice.Default, state.UserDefault, false)
	claims.Compact, claims.Long = previous.Compact, previous.Long
	if options.ManageCompact {
		claims.Compact = write(compactPath, compact, previous.Compact, choice.Compact, state.UserCompact, true)
		claims.Long = claims.Compact != "" && choice.Long
	}
	var removedObjects []string
	for _, id := range slices.Sorted(maps.Keys(previous.Budgets)) {
		if choice.Budgets[id] > 0 || !BudgetKeyOK(id) || budgetValue(per[id][defaultPath]) != strconv.Itoa(previous.Budgets[id]) {
			continue
		}
		if len(per[id]) == 1 {
			removedObjects = append(removedObjects, budgetObject(id))
		} else {
			remove = append(remove, BudgetPath(id))
		}
	}
	for _, id := range slices.Sorted(maps.Keys(choice.Budgets)) {
		if _, user := state.UserBudgets[BudgetKey(id)]; user || !BudgetKeyOK(id) {
			continue
		}
		value := choice.Budgets[id]
		if budgetValue(per[id][defaultPath]) != strconv.Itoa(value) {
			set = append(set, edit.KV{Path: BudgetPath(id), Value: value})
		}
		if claims.Budgets == nil {
			claims.Budgets = map[string]int{}
		}
		claims.Budgets[id] = value
	}
	if len(removedObjects) > 0 && len(removedObjects) == len(per) && len(claims.Budgets) == 0 {
		remove = append(remove, "modelSettings")
	} else {
		remove = append(remove, removedObjects...)
	}
	return set, remove, claims
}
