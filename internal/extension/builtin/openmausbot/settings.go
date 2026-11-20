package openmausbot

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/lazyre"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	ConfigFile  = "config.json"
	InstanceKey = "instances.magpie"
)

// DefaultFleet is OpenMausBot's DEFAULT_FLEET (server/instance-fleets.ts at
// 516ca75, API_KEY_FLEET in its place).
const DefaultFleet = `{
  "grok": {"driver": "grokAgent"},
  "kimi": {"driver": "kimiAgent"},
  "droid": {"driver": "droidAgent"},
  "cursor": {"driver": "cursorAgent"},
  "githubCopilot": {"driver": "copilotAgent"},
  "commandCode": {"driver": "commandCodeAgent"},
  "claude": {"driver": "claudeAgent"},
  "codex": {"driver": "codex"},
  "chatgpt": {"driver": "codex", "displayName": "ChatGPT plan", "config": {"authMode": "chatgpt-plan"}},
  "antigravity": {"driver": "antigravityAgent"},
  "opencodeGo": {"driver": "opencodeGo"},
  "openaiCompat": {"driver": "openai-compat"},
  "mistral": {"driver": "mistral"},
  "cerebras": {"driver": "cerebras"},
  "groq": {"driver": "groq"},
  "greenference": {"driver": "greenference"},
  "meta": {"driver": "meta"},
  "deepseek": {"driver": "deepseekAgent"},
  "museCode": {"driver": "museCode"},
  "openai": {"driver": "openai-compat", "displayName": "OpenAI", "access": "api", "icon": {"kind": "preset", "preset": "openai"},
    "config": {"url": "https://api.openai.com/v1", "apiKeyEnv": "OMB_OPENAI_API_KEY", "catalog": "openai"}},
  "claudeApi": {"driver": "claudeAgent", "displayName": "Claude (API key)", "access": "api", "icon": {"kind": "preset", "preset": "anthropic"},
    "config": {"requireApiKey": true}},
  "xaiApi": {"driver": "grok", "displayName": "xAI", "access": "api", "icon": {"kind": "preset", "preset": "xai"}},
  "openrouter": {"driver": "openai-compat", "displayName": "OpenRouter", "access": "api", "icon": {"kind": "preset", "preset": "openrouter"},
    "config": {"url": "https://openrouter.ai/api/v1", "apiKeyEnv": "OMB_OPENROUTER_API_KEY"}},
  "qwen": {"driver": "qwenAgent"},
  "hermes": {"driver": "hermesAgent"},
  "cline": {"driver": "clineAgent"},
  "pi": {"driver": "piAgent"}
}`

var modelID = lazyre.New(`(?i)^[a-z0-9][a-z0-9._:/-]*(?:\[1m\])?$`)

// AllowsModel applies CLAUDE_MODEL_ID to an endpoint model.
func AllowsModel(id string) bool { return modelID.MatchString(id) }

var routingEnvironment = []string{
	"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_CUSTOM_HEADERS",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
}

type SettingsIntent uint8

const (
	Connect SettingsIntent = iota + 1
	Sync
	Off
)

type SettingsOptions struct {
	Intent           SettingsIntent
	ClaudeHome       string
	Gateway          string
	Key              string
	Models           []string
	OwnsDefaultFleet bool
}

func PlanSettings(options SettingsOptions, settings sdk.ActionSettings) (sdk.AgentPlan, bool, error) {
	var plan sdk.AgentPlan
	if len(settings.Files) != 1 || settings.Files[0].Path != ConfigFile {
		return plan, false, errors.New("OpenMausBot settings plan requires its config snapshot")
	}
	file := settings.Files[0]
	instancesValue := gjson.GetBytes(jsonc.ToJSON(edit.TrimBOM(file.Body)), "instances")
	current := instancesValue.Get("magpie")
	switch options.Intent {
	case Connect:
		if len(options.Models) == 0 {
			return plan, false, errors.New("no magpie model OpenMausBot can list: pick models for OpenMausBot first")
		}
	case Sync:
		if !current.Exists() || len(options.Models) == 0 {
			return plan, false, nil
		}
	case Off:
		if len(bytes.TrimSpace(edit.TrimBOM(file.Body))) == 0 {
			return plan, false, nil
		}
	default:
		return plan, false, errors.New("unknown OpenMausBot settings intent")
	}

	var instances map[string]json.RawMessage
	if instancesValue.Exists() && (json.Unmarshal([]byte(instancesValue.Raw), &instances) != nil || instances == nil) && options.Intent != Off {
		return plan, false, edit.AtFile(file.Path, errors.New("instances isn't a map of engines, so magpie leaves it alone"))
	}
	var set []edit.KV
	var remove []string
	addedDefaultFleet := false
	if options.Intent == Off {
		delete(instances, "magpie")
		if options.OwnsDefaultFleet && edit.SameJSON(DefaultFleet, instances) {
			remove = []string{"instances"}
		} else if current.Exists() {
			remove = []string{InstanceKey}
		} else {
			_, err := edit.PatchJSON(file.Body, nil, []string{InstanceKey})
			return plan, false, edit.AtFile(file.Path, err)
		}
	} else {
		engine, err := mergeEngine(options, instances, current.Raw)
		if err != nil {
			return plan, false, edit.AtFile(file.Path, err)
		}
		if options.Intent == Sync && edit.SameJSON(current.Raw, engine) {
			return plan, false, nil
		}
		set = []edit.KV{{Path: InstanceKey, Value: engine}}
		if len(instances) == 0 {
			if err := json.Unmarshal([]byte(DefaultFleet), &instances); err != nil {
				return plan, false, err
			}
			body, err := json.Marshal(engine)
			if err != nil {
				return plan, false, err
			}
			instances["magpie"] = body
			set = []edit.KV{{Path: "instances", Value: instances}}
			addedDefaultFleet = true
		}
	}

	body, err := edit.PatchJSON(file.Body, set, remove)
	if err != nil {
		return plan, false, edit.AtFile(file.Path, err)
	}
	if !bytes.Equal(body, file.Body) {
		plan.Files = []sdk.FileChange{{Path: file.Path, BeforeHash: file.BeforeHash, Body: body}}
	}
	return plan, addedDefaultFleet, nil
}

func mergeEngine(options SettingsOptions, instances map[string]json.RawMessage, current string) (any, error) {
	config := map[string]any{
		"configDir": options.ClaudeHome, "requireApiKey": true, "endpointModels": options.Models,
	}
	ids := make([]string, 0, len(instances))
	for id := range instances {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if id == "magpie" {
			continue
		}
		var engine struct {
			Driver string         `json:"driver"`
			Config map[string]any `json:"config"`
		}
		if json.Unmarshal(instances[id], &engine) != nil || engine.Driver != "claudeAgent" {
			continue
		}
		if _, endpoint := engine.Config["endpointModels"]; endpoint {
			continue
		}
		if cli, _ := engine.Config["cli"].(string); cli != "" {
			config["cli"] = cli
			break
		}
	}
	env := map[string]any{
		"ANTHROPIC_BASE_URL": options.Gateway, "ANTHROPIC_AUTH_TOKEN": options.Key,
		"ANTHROPIC_API_KEY": options.Key, "ANTHROPIC_MODEL": options.Models[0],
	}
	want := map[string]any{
		"driver": "claudeAgent", "displayName": "magpie", "access": "custom",
		"config": config, "environment": env,
	}
	var have map[string]json.RawMessage
	if json.Unmarshal([]byte(current), &have) != nil || have == nil {
		return want, nil
	}
	var theirs map[string]any
	if json.Unmarshal(have["environment"], &theirs) == nil {
		for name, value := range theirs {
			text, isText := value.(string)
			if !isText || slices.Contains(routingEnvironment, name) {
				continue
			}
			if name != "ANTHROPIC_MODEL" || slices.Contains(options.Models, text) {
				env[name] = text
			}
		}
	}
	if _, named := have["displayName"]; named {
		delete(want, "displayName")
	}
	body, err := json.Marshal(want)
	if err != nil {
		return nil, err
	}
	var replacement map[string]json.RawMessage
	if err := json.Unmarshal(body, &replacement); err != nil {
		return nil, err
	}
	return edit.MergeJSONObjects(have, replacement, []string{"environment"}), nil
}
