package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/tidwall/jsonc"
	"gopkg.in/yaml.v3"

	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type Format uint8

const (
	JSON Format = iota
	TOMLTop
	YAMLTop
	CodexTOML
)

type Key struct {
	Name  string
	Value any
	Off   func(any) bool
}

func (key Key) Matches(value any) bool {
	current, _ := json.Marshal(value)
	wanted, _ := json.Marshal(key.Value)
	return bytes.Equal(current, wanted)
}

type Lookup func(name string) (any, bool)

func (format Format) Decode(raw []byte) (Lookup, error) {
	raw = edit.TrimBOM(raw)
	var fields map[string]any
	switch format {
	case JSON:
		body := jsonc.ToJSON(raw)
		if len(bytes.TrimSpace(body)) != 0 {
			var value any
			if err := json.Unmarshal(body, &value); err != nil {
				return nil, err
			}
			fields, _ = value.(map[string]any)
		}
		return func(name string) (any, bool) {
			var current any = fields
			for _, part := range strings.Split(name, ".") {
				table, ok := current.(map[string]any)
				if !ok {
					return nil, false
				}
				current, ok = table[part]
				if !ok {
					return nil, false
				}
			}
			return current, true
		}, nil
	case TOMLTop, YAMLTop:
		if len(bytes.TrimSpace(raw)) != 0 {
			decode := toml.Unmarshal
			if format == YAMLTop {
				decode = yaml.Unmarshal
			}
			if err := decode(raw, &fields); err != nil {
				return nil, err
			}
		}
		return func(name string) (any, bool) {
			value, exists := fields[name]
			return value, exists
		}, nil
	case CodexTOML:
		var config struct {
			Analytics any `toml:"analytics"`
		}
		if err := toml.Unmarshal(edit.TOMLWithout(raw, "projects"), &config); err != nil {
			return nil, err
		}
		return Analytics(config.Analytics)
	default:
		return nil, errors.New("unknown telemetry settings format")
	}
}

func Analytics(value any) (Lookup, error) {
	table, ok := value.(map[string]any)
	if !ok && value != nil {
		return nil, errors.New("analytics is not a table")
	}
	return func(name string) (any, bool) {
		if name == "analytics" {
			return value, value != nil
		}
		entry, exists := table[strings.TrimPrefix(name, "analytics.")]
		return entry, exists
	}, nil
}

type SettingsOptions struct {
	Format      Format
	Off         bool
	Keys        []Key
	Previous    string
	DisplayPath string
}

func PlanSettings(options SettingsOptions, settings sdk.ActionSettings) (sdk.AgentPlan, string, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != "config" {
		return sdk.AgentPlan{}, "", errors.New("telemetry action requires a config snapshot")
	}
	file := settings.Files[0]
	get, err := options.Format.Decode(file.Body)
	if err != nil {
		return sdk.AgentPlan{}, "", edit.AtFile(file.Path, err)
	}
	previous := strings.Split(options.Previous, ",")
	var set []edit.KV
	var remove, owned []string
	for _, key := range options.Keys {
		value, exists := get(key.Name)
		mine := exists && slices.Contains(previous, key.Name) && key.Matches(value)
		if options.Off {
			switch {
			case !exists:
				set = append(set, edit.KV{Path: key.Name, Value: key.Value})
				owned = append(owned, key.Name)
			case !key.Off(value):
				encoded, _ := json.Marshal(value)
				return sdk.AgentPlan{}, "", fmt.Errorf("%s sets %s to %s; magpie leaves a value of yours as it is (change it there)", options.DisplayPath, key.Name, encoded)
			case mine:
				owned = append(owned, key.Name)
			}
		} else if mine {
			remove = append(remove, key.Name)
		}
	}
	next := strings.Join(owned, ",")
	if len(set) == 0 && len(remove) == 0 {
		return sdk.AgentPlan{}, next, nil
	}
	_, hasAnalytics := get("analytics")
	raw := edit.TrimBOM(file.Body)
	body, err := options.patch(file.Path, raw, hasAnalytics, set, remove)
	if err != nil || bytes.Equal(body, raw) {
		return sdk.AgentPlan{}, next, err
	}
	return sdk.AgentPlan{Files: []sdk.FileChange{{Path: file.Path, BeforeHash: file.BeforeHash, Body: body}}}, next, nil
}

func (options SettingsOptions) patch(path string, raw []byte, hasAnalytics bool, set []edit.KV, remove []string) ([]byte, error) {
	switch options.Format {
	case JSON:
		body, err := edit.PatchJSON(raw, set, remove)
		return body, edit.AtFile(path, err)
	case TOMLTop:
		if len(set) != 0 {
			return edit.RewriteTOMLTop(path, raw, set...)
		}
		return edit.RewriteTOMLWithoutTop(path, raw, remove...)
	case YAMLTop:
		if len(set) != 0 {
			return edit.RewriteYAMLTop(path, raw, set...)
		}
		return edit.RewriteYAMLWithoutTop(path, raw, remove...)
	case CodexTOML:
		if len(set) != 0 {
			hasTable, err := edit.HasTOMLTable(raw, "analytics")
			if err != nil {
				return nil, edit.AtFile(path, err)
			}
			if !hasTable && hasAnalytics {
				return nil, fmt.Errorf("%s has analytics written in a way magpie doesn't add to; set analytics.enabled there", options.DisplayPath)
			}
			return edit.RewriteTOMLKey(path, raw, "analytics", "enabled", set[0].Value)
		}
		return edit.RewriteTOMLWithoutKey(path, raw, "analytics", "enabled")
	default:
		return nil, errors.New("unknown telemetry settings format")
	}
}
