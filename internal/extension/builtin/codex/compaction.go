package codex

import (
	"bytes"
	"errors"

	"github.com/pelletier/go-toml/v2"
	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	AsyncCompactionTable   = "features.async_compaction"
	AsyncCompactionPercent = "model_async_compact_threshold_percent"
)

type AsyncCompactionState struct {
	Enabled bool `json:"enabled,omitempty"`
	Percent int  `json:"percent,omitempty"`
	Cap     bool `json:"cap,omitempty"`
	Bool    bool `json:"bool,omitempty"`
}

type AsyncCompactionOptions struct {
	Routed  bool
	On      bool
	Percent int
	TableOK bool
	Prior   AsyncCompactionState
}

func PlanAsyncCompaction(options AsyncCompactionOptions, settings sdk.ActionSettings) (sdk.AgentPlan, AsyncCompactionState, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != GlobalConfigFile {
		return sdk.AgentPlan{}, options.Prior, errors.New("Codex compaction requires a config.toml snapshot")
	}
	file := settings.Files[0]
	original := edit.TrimBOM(file.Body)
	body, next, err := planAsyncSettings(file.Path, original, options)
	if err != nil || bytes.Equal(original, body) && (next == options.Prior || !file.Exists) {
		return sdk.AgentPlan{}, next, edit.AtFile(file.Path, err)
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: file.Path, Body: body, BeforeHash: file.BeforeHash}}}, next, nil
}

func asyncSettings(raw []byte) (any, map[string]any, error) {
	var config struct {
		Features       any `toml:"features"`
		ModelProviders any `toml:"model_providers"`
	}
	if err := toml.Unmarshal(edit.TOMLWithout(raw, "projects"), &config); err != nil {
		return nil, nil, err
	}
	features, _ := config.Features.(map[string]any)
	providers, _ := config.ModelProviders.(map[string]any)
	provider, _ := providers["magpie"].(map[string]any)
	return features["async_compaction"], provider, nil
}

func planAsyncSettings(path string, raw []byte, options AsyncCompactionOptions) ([]byte, AsyncCompactionState, error) {
	feature, provider, err := asyncSettings(raw)
	state := options.Prior
	var bad *toml.DecodeError
	if errors.As(err, &bad) {
		return raw, state, nil
	}
	if err != nil {
		return nil, state, err
	}
	want := options.Routed && options.On
	useTable := want && options.TableOK
	percent := options.Percent
	if !useTable {
		percent = 0
	}

	set := func(table, key string, value any) error {
		raw, err = edit.RewriteTOMLKey(path, raw, table, key, value)
		return err
	}
	remove := func(table, key string) error {
		raw, err = edit.RewriteTOMLWithoutKey(path, raw, table, key)
		return err
	}

	if state.Bool && (!want || useTable) {
		if feature == true {
			if err := remove("features", "async_compaction"); err != nil {
				return nil, state, err
			}
			if feature, _, err = asyncSettings(raw); err != nil {
				return nil, state, err
			}
		}
		state.Bool = false
	}

	beforeTable := raw
	table, _ := feature.(map[string]any)
	asTable := feature == nil
	if table != nil {
		asTable, err = edit.HasTOMLTable(raw, AsyncCompactionTable)
		if err != nil {
			return nil, state, err
		}
	}
	if asTable {
		enabled, hasEnabled := table["enabled"].(bool)
		if table["enabled"] != nil && !(hasEnabled && state.Enabled && enabled) {
			state.Enabled = false
		}
		switch {
		case useTable && table["enabled"] == nil:
			if err := set(AsyncCompactionTable, "enabled", true); err != nil {
				return nil, state, err
			}
			state.Enabled = true
		case !useTable && state.Enabled:
			if err := remove(AsyncCompactionTable, "enabled"); err != nil {
				return nil, state, err
			}
			state.Enabled = false
		}
		cur, hasPercent := table[AsyncCompactionPercent].(int64)
		if table[AsyncCompactionPercent] != nil && !(hasPercent && int64(state.Percent) == cur) {
			state.Percent = 0
		}
		switch {
		case percent > 0 && (table[AsyncCompactionPercent] == nil || state.Percent != 0):
			if state.Percent != percent {
				if err := set(AsyncCompactionTable, AsyncCompactionPercent, percent); err != nil {
					return nil, state, err
				}
			}
			state.Percent = percent
		case percent == 0 && state.Percent != 0:
			if err := remove(AsyncCompactionTable, AsyncCompactionPercent); err != nil {
				return nil, state, err
			}
			state.Percent = 0
		}
	} else {
		state.Enabled, state.Percent = false, 0
	}

	if !bytes.Equal(beforeTable, raw) {
		if feature, _, err = asyncSettings(raw); err != nil {
			return nil, state, err
		}
	}
	switch {
	case want && !useTable && feature == nil:
		if err := set("features", "async_compaction", true); err != nil {
			return nil, state, err
		}
		state.Bool, feature = true, true
	case state.Bool && feature != true:
		state.Bool = false
	}

	table, _ = feature.(map[string]any)
	if feature == true || table["enabled"] == true {
		want = options.Routed
	}
	if provider == nil {
		state.Cap = false
		return raw, state, nil
	}
	caps, has := provider["capabilities"].(map[string]any)
	if has && !(state.Cap && len(caps) == 1 && caps["remote_compaction"] == "v2") {
		state.Cap = false
	}
	switch {
	case want && provider["capabilities"] == nil:
		if err := set(ProviderTablePath, "capabilities", edit.Inline{{Path: "remote_compaction", Value: "v2"}}); err != nil {
			return nil, state, err
		}
		state.Cap = true
	case !want && state.Cap:
		if err := remove(ProviderTablePath, "capabilities"); err != nil {
			return nil, state, err
		}
		state.Cap = false
	}
	return raw, state, nil
}
