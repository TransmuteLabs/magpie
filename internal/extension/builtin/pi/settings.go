package pi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	SettingsFile = "settings.json"
	CatalogFile  = "models.json"
)

var thinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

func ThinkingLevels() []string { return slices.Clone(thinkingLevels) }

func Bare(pattern string) string {
	if index := strings.LastIndex(pattern, ":"); index >= 0 && slices.Contains(thinkingLevels, pattern[index+1:]) {
		return pattern[:index]
	}
	return pattern
}

func Takes(pattern, ref string) bool {
	glob := strings.ContainsAny(pattern, "*?[")
	pattern, ref = strings.ToLower(strings.TrimSpace(Bare(pattern))), strings.ToLower(ref)
	if pattern == ref {
		return true
	}
	if !glob {
		return false
	}
	_, model, _ := strings.Cut(ref, "/")
	for _, value := range []string{ref, model} {
		if ok, _ := path.Match(pattern, value); ok {
			return true
		}
	}
	return false
}

func Scope(raw string) []string {
	var patterns []string
	if json.Unmarshal([]byte(raw), &patterns) != nil || len(patterns) == 0 {
		return nil
	}
	return patterns
}

func PlanModel(model string, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	fileCount := 1
	if model == "" {
		fileCount = 2
	}
	if len(settings.Files) != fileCount || settings.Files[0].Path != SettingsFile ||
		fileCount == 2 && settings.Files[1].Path != CatalogFile {
		return sdk.AgentPlan{}, errors.New("Pi model action requires settings and, on reset, the catalog")
	}
	file := settings.Files[0]
	current := parseSettings(file.Body)
	patterns := Scope(current.Get("enabledModels").Raw)
	var plan sdk.AgentPlan
	if model == "" {
		remove := []string{"defaultProvider", "defaultModel"}
		var set []edit.KV
		kept := slices.DeleteFunc(slices.Clone(patterns), func(pattern string) bool {
			return !strings.ContainsAny(pattern, "*?[") && strings.HasPrefix(strings.ToLower(pattern), "magpie/")
		})
		if len(kept) != len(patterns) {
			if len(kept) == 0 {
				remove = append(remove, "enabledModels")
			} else {
				set = append(set, edit.KV{Path: "enabledModels", Value: kept})
			}
		}
		if err := patch(&plan, file, set, remove); err != nil {
			return sdk.AgentPlan{}, err
		}
		if err := patch(&plan, settings.Files[1], nil, []string{"providers.magpie"}); err != nil {
			return sdk.AgentPlan{}, err
		}
		return plan, nil
	}
	provider, id, ok := strings.Cut(model, "/")
	if !ok || provider == "" || id == "" {
		return sdk.AgentPlan{}, fmt.Errorf("expected provider/model, got %q", model)
	}
	set := []edit.KV{{Path: "defaultProvider", Value: provider}, {Path: "defaultModel", Value: id}}
	if patterns != nil && !slices.ContainsFunc(patterns, func(pattern string) bool { return Takes(pattern, model) }) {
		set = append(set, edit.KV{Path: "enabledModels", Value: append(patterns, model)})
	}
	if err := patch(&plan, file, set, nil); err != nil {
		return sdk.AgentPlan{}, err
	}
	return plan, nil
}

func PlanEffort(level string, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != SettingsFile {
		return sdk.AgentPlan{}, errors.New("Pi effort action requires a settings.json snapshot")
	}
	const key = "defaultThinkingLevel"
	file := settings.Files[0]
	if parseSettings(file.Body).Get(key).String() == level {
		return sdk.AgentPlan{}, nil
	}
	var plan sdk.AgentPlan
	var err error
	if level == "" {
		err = patch(&plan, file, nil, []string{key})
	} else {
		err = patch(&plan, file, []edit.KV{{Path: key, Value: level}}, nil)
	}
	return plan, err
}

type ScopeSettings struct {
	Patterns []string
	Present  bool
	Default  string
	Offered  []string
}

func PlanScope(update func(ScopeSettings) ([]string, bool), settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	files := settings.Files
	if len(files) < 1 || len(files) > 2 || files[0].Path != SettingsFile ||
		len(files) == 2 && files[1].Path != CatalogFile {
		return sdk.AgentPlan{}, errors.New("Pi scope action requires settings and an optional catalog")
	}
	file := files[0]
	current := parseSettings(file.Body)
	scope := current.Get("enabledModels")
	state := ScopeSettings{Patterns: Scope(scope.Raw), Present: scope.Exists()}
	if state.Present && state.Patterns == nil {
		return sdk.AgentPlan{}, nil
	}
	if provider, model := current.Get("defaultProvider").String(), current.Get("defaultModel").String(); provider != "" && model != "" {
		state.Default = provider + "/" + model
	}
	if len(files) == 2 {
		var models []struct {
			ID string `json:"id"`
		}
		catalog := parseSettings(files[1].Body).Get("providers.magpie.models")
		if json.Unmarshal([]byte(catalog.Raw), &models) != nil {
			return sdk.AgentPlan{}, nil
		}
		for _, model := range models {
			if model.ID != "" {
				state.Offered = append(state.Offered, model.ID)
			}
		}
	}
	patterns, changed := update(state)
	if !changed {
		return sdk.AgentPlan{}, nil
	}
	var plan sdk.AgentPlan
	var err error
	if len(patterns) == 0 {
		err = patch(&plan, file, nil, []string{"enabledModels"})
	} else {
		err = patch(&plan, file, []edit.KV{{Path: "enabledModels", Value: patterns}}, nil)
	}
	return plan, err
}

func parseSettings(body []byte) gjson.Result {
	return gjson.ParseBytes(jsonc.ToJSON(edit.TrimBOM(body)))
}

func patch(plan *sdk.AgentPlan, file sdk.ActionSettingFile, set []edit.KV, remove []string) error {
	if len(set) == 0 && len(bytes.TrimSpace(edit.TrimBOM(file.Body))) == 0 {
		return nil
	}
	body, err := edit.PatchJSON(file.Body, set, remove)
	if err != nil {
		return edit.AtFile(file.Path, err)
	}
	if !bytes.Equal(body, file.Body) {
		plan.Files = append(plan.Files, sdk.FileChange{Path: file.Path, BeforeHash: file.BeforeHash, Body: body})
	}
	return nil
}
