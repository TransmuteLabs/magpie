package grok

import (
	"slices"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
)

type ModelSettings struct {
	Windows, Percents map[string]int
	SessionPercent    bool
}

func ReadModelSettings(raw []byte) (ModelSettings, error) {
	var doc struct {
		Model   map[string]map[string]any `toml:"model"`
		Session map[string]any            `toml:"session"`
	}
	if err := toml.Unmarshal(edit.TrimBOM(raw), &doc); err != nil {
		return ModelSettings{}, err
	}
	_, sessionPercent := doc.Session["auto_compact_threshold_percent"]
	had := ModelSettings{Windows: map[string]int{}, Percents: map[string]int{}, SessionPercent: sessionPercent}
	for name, table := range doc.Model {
		id, own := strings.CutPrefix(name, "magpie/")
		if !own {
			continue
		}
		if value, ok := table["context_window"].(int64); ok {
			had.Windows[id] = int(value)
		}
		if value, ok := table["auto_compact_threshold_percent"].(int64); ok {
			had.Percents[id] = int(value)
		}
	}
	return had, nil
}

func modelTables(visible Catalog, had ModelSettings, previousWindows, previousPercents map[string]int) ([]edit.Table, map[string]int, map[string]int) {
	var tables []edit.Table
	windows, percents := map[string]int{}, map[string]int{}
	for _, model := range visible.Models {
		kvs := []edit.KV{
			{Path: "model", Value: model.ID},
			{Path: "name", Value: model.Name},
			{Path: "base_url", Value: visible.URL},
			{Path: "api_key", Value: visible.Key},
			{Path: "api_backend", Value: backend(model)},
		}
		window := model.Context
		if window > 0 {
			windows[model.ID] = window
			if value := had.Windows[model.ID]; value > 0 && value != window && value != previousWindows[model.ID] {
				window = value
			}
			kvs = append(kvs, edit.KV{Path: "context_window", Value: window})
		}
		value, set := had.Percents[model.ID]
		previous, recorded := previousPercents[model.ID]
		if set && value >= 0 && value <= 100 && (!recorded || value != previous) {
			kvs = append(kvs, edit.KV{Path: "auto_compact_threshold_percent", Value: value})
		} else if value := compactPercent(visible, model, window); value > 0 && !had.SessionPercent {
			percents[model.ID] = value
			kvs = append(kvs, edit.KV{Path: "auto_compact_threshold_percent", Value: value})
		}
		var allowed []string
		for _, effort := range efforts {
			if slices.Contains(model.Efforts, effort) {
				allowed = append(allowed, strconv.Quote(effort))
			}
		}
		if len(allowed) > 0 {
			kvs = append(kvs, edit.KV{Path: "reasoning_efforts", Value: edit.Raw("[" + strings.Join(allowed, ", ") + "]")})
		}
		tables = append(tables, edit.Table{Name: "model." + strconv.Quote("magpie/"+model.ID), KVs: kvs})
	}
	return tables, windows, percents
}

func compactPercent(visible Catalog, model catalog.Model, window int) int {
	if model.Context <= 0 || window <= 0 {
		return 0
	}
	tokens := min(model.Context, model.Compact)
	if tokens <= 0 {
		tokens = visible.Working(model.Context)
	}
	if tokens <= 0 || tokens >= window {
		return 0
	}
	return max(1, tokens*100/window)
}

func backend(model catalog.Model) string {
	switch {
	case slices.Contains(model.APIs, "responses"):
		return "responses"
	case slices.Contains(model.APIs, "anthropic"):
		return "messages"
	}
	return "chat_completions"
}
