package qwen

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

const (
	SettingsFile = "settings.json"
	Model        = "model.name"
	ModelBase    = "model.baseUrl"
	Auth         = "security.auth.selectedType"
	AuthSeen     = Auth + ".seen"
	EnvKey       = "MAGPIE_QWEN_API_KEY"
	Env          = "env." + EnvKey
	models       = "modelProviders.openai"
)

type Entry struct {
	BaseURL    string      `json:"baseUrl"`
	EnvKey     string      `json:"envKey"`
	Generation *Generation `json:"generationConfig,omitempty"`
	ID         string      `json:"id"`
	Name       string      `json:"name"`
}

type Generation struct {
	ContextWindowSize int `json:"contextWindowSize,omitempty"`
}

type Custom struct {
	Raw      json.RawMessage
	ID, Name string
	Base     string
	Owned    bool
}

func Customs(clean []byte) []Custom {
	var out []Custom
	if entries := gjson.GetBytes(clean, models); entries.IsArray() {
		entries.ForEach(func(_, value gjson.Result) bool {
			out = append(out, Custom{
				Raw: json.RawMessage(value.Raw), ID: value.Get("id").String(),
				Name: value.Get("name").String(), Base: value.Get("baseUrl").String(),
				Owned: value.Get("envKey").String() == EnvKey,
			})
			return true
		})
	}
	return out
}

func Find(entries []Custom, name, base string) *Custom {
	for index := range entries {
		entry := &entries[index]
		if entry.ID == name && (base == "" || entry.Base == base) {
			return entry
		}
	}
	return nil
}

type Options struct {
	Sync     bool
	Through  bool
	Choice   string
	Gateway  string
	Key      string
	Catalog  func() []Entry
	Previous map[string]string
}

type Recovery struct {
	Save   map[string]string
	Forget []string
}

func Plan(options Options, settings sdk.ActionSettings) (sdk.AgentPlan, Recovery, error) {
	if len(settings.Files) != 1 || settings.Files[0].Path != SettingsFile {
		return sdk.AgentPlan{}, Recovery{}, errors.New("Qwen Code plan needs settings.json")
	}
	file := settings.Files[0]
	raw := edit.TrimBOM(file.Body)
	clean := jsonc.ToJSON(raw)
	if len(bytes.TrimSpace(clean)) != 0 && (!json.Valid(clean) || !gjson.ParseBytes(clean).IsObject()) {
		return sdk.AgentPlan{}, Recovery{}, edit.AtFile(file.Path, errors.New("invalid JSON/JSONC settings object"))
	}
	customs := Customs(clean)
	var entries []any
	var hadOwned bool
	for _, entry := range customs {
		if entry.Owned {
			hadOwned = true
			continue
		}
		entries = append(entries, entry.Raw)
	}
	if options.Sync && !hadOwned {
		return sdk.AgentPlan{}, Recovery{}, nil
	}
	recovery := Recovery{Save: map[string]string{}}
	var set []edit.KV
	var remove []string
	var prune []string
	setValue := func(path string, value any) {
		set = append(set, edit.KV{Path: path, Value: value})
	}
	name := gjson.GetBytes(clean, Model).String()
	base := gjson.GetBytes(clean, ModelBase).String()
	active := Find(customs, name, base)
	onGateway := active != nil && active.Owned
	if options.Through || options.Sync {
		if value := gjson.GetBytes(clean, Env); value.Exists() && value.String() != options.Key {
			recovery.Save[Env] = value.String()
		}
		if options.Previous[AuthSeen] == "" {
			recovery.Save[AuthSeen] = "1"
			if value := gjson.GetBytes(clean, Auth); value.Exists() {
				recovery.Save[Auth] = value.String()
			}
		}
		setValue(Env, options.Key)
		setValue(Auth, "openai")
		for _, entry := range options.Catalog() {
			entries = append(entries, entry)
		}
		if !options.Sync && !onGateway {
			recovery.Save[Model] = name
			recovery.Save[ModelBase] = base
		}
	} else {
		if restored := options.Previous[Env]; restored != "" {
			setValue(Env, restored)
		} else if gjson.GetBytes(clean, Env).String() == options.Key {
			remove = append(remove, Env)
			prune = append(prune, "env")
		}
		if hadOwned || options.Previous[AuthSeen] != "" {
			if gjson.GetBytes(clean, Auth).String() == "openai" {
				if restored := options.Previous[Auth]; restored != "" {
					setValue(Auth, restored)
				} else {
					remove = append(remove, Auth)
					prune = append(prune, "security.auth", "security")
				}
			}
			recovery.Forget = append(recovery.Forget, Auth, AuthSeen)
		}
		recovery.Forget = append(recovery.Forget, Env)
	}
	if options.Through || options.Sync || hadOwned {
		if len(entries) != 0 {
			setValue(models, entries)
		} else {
			remove = append(remove, models)
		}
		if hadOwned && !options.Through && !options.Sync {
			prune = append(prune, "modelProviders")
		}
	}
	if !options.Sync {
		name, base = options.Choice, ""
		switch {
		case options.Through:
			base = options.Gateway
		case onGateway && name == "":
			name, base = options.Previous[Model], options.Previous[ModelBase]
		default:
			if match := Find(customs, name, ""); name != "" && match != nil {
				base = match.Base
			}
		}
		if onGateway && !options.Through {
			recovery.Forget = append(recovery.Forget, Model, ModelBase)
		}
		if name != "" {
			if base != "" {
				setValue(ModelBase, base)
			} else {
				remove = append(remove, ModelBase)
			}
			setValue(Model, name)
		} else {
			remove = append(remove, ModelBase, Model)
			prune = append(prune, "model")
		}
	}
	if len(bytes.TrimSpace(raw)) == 0 && len(set) == 0 {
		return sdk.AgentPlan{}, recovery, nil
	}
	body, err := edit.PatchJSON(file.Body, set, remove)
	if err != nil {
		return sdk.AgentPlan{}, Recovery{}, edit.AtFile(file.Path, err)
	}
	for _, parent := range prune {
		if value := gjson.GetBytes(jsonc.ToJSON(edit.TrimBOM(body)), parent); strings.TrimSpace(value.Raw) == "{}" {
			body, err = edit.PatchJSON(body, nil, []string{parent})
			if err != nil {
				return sdk.AgentPlan{}, Recovery{}, edit.AtFile(file.Path, err)
			}
		}
	}
	plan := sdk.AgentPlan{}
	if !bytes.Equal(file.Body, body) {
		plan.Files = []sdk.FileChange{{Path: file.Path, BeforeHash: file.BeforeHash, Body: body}}
	}
	return plan, recovery, nil
}
