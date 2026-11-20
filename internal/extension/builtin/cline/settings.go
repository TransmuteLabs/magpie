package cline

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/edit"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	ProviderSlot = "openai-compatible"
	StateFile    = "globalState.json"
	SecretsFile  = "secrets.json"
	ModelsFile   = "models.json"
)

type Model struct {
	ID, Name        string
	Context, Output int
	Images          bool
	Reasoning       bool
}

type GatewaySettings struct {
	URL    func() string
	Key    func() string
	OwnKey func(string) bool
	Models func() []Model
}

func checkFiles(settings sdk.ActionSettings) error {
	files := settings.Files
	if len(files) != 4 || files[0].Path != ProvidersFile || files[1].Path != StateFile ||
		files[2].Path != SecretsFile || files[3].Path != ModelsFile {
		return errors.New("Cline settings plan needs providers.json, globalState.json, secrets.json and models.json")
	}
	return nil
}

func PlanSync(gateway GatewaySettings, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	var plan sdk.AgentPlan
	if err := checkFiles(settings); err != nil {
		return plan, err
	}
	providers, state, secrets, models := settings.Files[0], settings.Files[1], settings.Files[2], settings.Files[3]
	const slot = "providers." + ProviderSlot
	const prefix = slot + ".settings."
	config := readSettings(providers)
	if config.Get("lastUsedProvider").String() != ProviderSlot || !gateway.OwnKey(config.Get(prefix+"apiKey").String()) {
		return plan, nil
	}
	update := func(file sdk.ActionSettingFile, key string, current gjson.Result, value any) error {
		if !current.Exists() || edit.SameJSON(current.Raw, value) {
			return nil
		}
		body, err := edit.PatchJSON(file.Body, []edit.KV{{Path: key, Value: value}}, nil)
		if err != nil {
			return edit.AtFile(file.Path, err)
		}
		plan.Files = append(plan.Files, sdk.FileChange{Path: file.Path, BeforeHash: file.BeforeHash, Body: body})
		return nil
	}
	v1 := gateway.URL()
	if err := update(providers, prefix+"baseUrl", config.Get(prefix+"baseUrl"), v1); err != nil {
		return plan, err
	}
	if gateway.OwnKey(readSettings(secrets).Get("openAiApiKey").String()) {
		const stateURL = "openAiBaseUrl"
		if err := update(state, stateURL, readSettings(state).Get(stateURL), v1); err != nil {
			return plan, err
		}
	}
	if current := readSettings(models).Get(slot); current.Exists() {
		want := Catalog(config.Get(prefix+"model").String(), v1, gateway.Models())
		if err := update(models, slot, current, keptCatalog(current.String(), want)); err != nil {
			return plan, err
		}
	}
	return plan, nil
}

func readSettings(file sdk.ActionSettingFile) gjson.Result {
	return gjson.ParseBytes(jsonc.ToJSON(edit.TrimBOM(file.Body)))
}

func providerEntry(model, effort, url, key string) map[string]any {
	s := map[string]any{
		"provider": ProviderSlot, "apiKey": key, "model": model, "baseUrl": url,
		"headers": map[string]string{"User-Agent": "cline"},
	}
	if effort != "" {
		s["reasoning"] = reasoning(effort)
	}
	return map[string]any{"settings": s, "updatedAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), "tokenSource": "manual"}
}

// stateKeys are the keys of the extension's globalState.json magpie
// sets.
var stateKeys = []string{"actModeApiProvider", "planModeApiProvider", "actModeOpenAiModelId", "planModeOpenAiModelId",
	"actModeOpenAiModelInfo", "planModeOpenAiModelInfo", "openAiBaseUrl", "openAiHeaders"}

func Catalog(model, v1 string, models []Model) map[string]any {
	ms := map[string]any{}
	for _, m := range models {
		caps := []string{"streaming", "tools"}
		if m.Images {
			caps = append(caps, "images")
		}
		if m.Reasoning {
			caps = append(caps, "reasoning")
		}
		e := map[string]any{"id": m.ID, "name": m.Name, "capabilities": caps}
		if m.Context > 0 {
			e["contextWindow"] = m.Context
		}
		if m.Output > 0 {
			e["maxTokens"] = m.Output
		}
		ms[m.ID] = e
	}
	return map[string]any{
		"provider": map[string]any{"name": "magpie", "baseUrl": v1, "defaultModelId": model},
		"models":   ms,
	}
}

// keptCatalog is magpie's entry want laid over the one in Cline's models.json.
// What the user set there stays (#2030): a key of the provider's
// beside its name, address and default model (protocol, client,
// capabilities, modelsSourceUrl), and on each of magpie's models what
// magpie doesn't write, such as an override from the VS Code extension's
// model editor (temperature, prices, apiFormat, supportsVision;
// sdk/packages/core/src/services/providers/local-provider-registry.ts). A
// model magpie no longer lists goes, with what was set on it.
func keptCatalog(cur string, want map[string]any) any {
	var have map[string]any
	if json.Unmarshal([]byte(cur), &have) != nil || have == nil {
		return want
	}
	hm, _ := have["models"].(map[string]any)
	wm, _ := want["models"].(map[string]any)
	ms := make(map[string]any, len(wm))
	for id, w := range wm {
		h, _ := hm[id].(map[string]any)
		if h == nil {
			ms[id] = w
			continue
		}
		ms[id] = edit.MergeObjects(h, w.(map[string]any), nil, nil)
	}
	out := edit.MergeObjects(have, want, []string{"models"}, nil)
	out["models"] = ms
	return out
}

type ModelOptions struct {
	Gateway          GatewaySettings
	Model            string
	ResolveReference func(string) (string, bool)
	Saved, Keep      map[string]string
	Restoring        bool
}

func PlanModel(pick *ModelOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	var plan sdk.AgentPlan
	if err := checkFiles(settings); err != nil {
		return plan, err
	}
	providers, state, secrets, models := settings.Files[0], settings.Files[1], settings.Files[2], settings.Files[3]
	const slot = "providers." + ProviderSlot
	const lastUsed = "lastUsedProvider"
	const secretKey = "openAiApiKey"
	config := readSettings(providers)
	last := config.Get(lastUsed).String()
	onMagpie := last == ProviderSlot && pick.Gateway.OwnKey(config.Get(slot+".settings.apiKey").String())
	own := last
	if own == "" {
		own = "cline"
	}
	patch := func(file sdk.ActionSettingFile, body []byte, set []edit.KV, del []string) ([]byte, error) {
		if len(set) == 0 && (len(del) == 0 || len(bytes.TrimSpace(edit.TrimBOM(body))) == 0) {
			return body, nil
		}
		out, err := edit.PatchJSON(body, set, del)
		return out, edit.AtFile(file.Path, err)
	}
	add := func(file sdk.ActionSettingFile, body []byte) {
		if !bytes.Equal(file.Body, body) {
			plan.Files = append(plan.Files, sdk.FileChange{Path: file.Path, BeforeHash: file.BeforeHash, Body: body})
		}
	}
	change := func(file sdk.ActionSettingFile, initial string, set []edit.KV, del []string) error {
		body := file.Body
		if !file.Exists && initial != "" {
			body = []byte(initial + "\n")
		}
		body, err := patch(file, body, set, del)
		if err == nil {
			add(file, body)
		}
		return err
	}
	restore := func(file sdk.ActionSettingFile, key, saved string, value any) error {
		if saved != "" {
			return change(file, "", []edit.KV{{Path: key, Value: value}}, nil)
		}
		return change(file, "", nil, []string{key})
	}
	saved := func(key string) string { return pick.Saved[key] }
	keep := func(key, value string) { pick.Keep[key] = value }
	secret := readSettings(secrets).Get(secretKey).String()
	stateConfig := readSettings(state)
	v1 := pick.Gateway.URL()
	stateOnMagpie := pick.Gateway.OwnKey(secret) && stateConfig.Get("openAiBaseUrl").String() == v1
	if ref, ok := pick.ResolveReference(pick.Model); ok {
		catalog := readSettings(models).Get(slot).String()
		if !onMagpie {
			keep("entry", config.Get(slot).String())
			keep(lastUsed, last)
			keep("models", catalog)
		}
		if err := change(models, `{"version":1,"providers":{}}`, []edit.KV{
			{Path: slot, Value: keptCatalog(catalog, Catalog(ref, v1, pick.Gateway.Models()))},
		}, nil); err != nil {
			return plan, err
		}
		if state.Exists {
			if !stateOnMagpie {
				snapshot := make(map[string]json.RawMessage)
				for _, key := range stateKeys {
					if value := stateConfig.Get(key); value.Exists() {
						snapshot[key] = json.RawMessage(value.Raw)
					}
				}
				body, err := json.Marshal(snapshot)
				if err != nil {
					return plan, edit.AtFile(state.Path, err)
				}
				keep("state", string(body))
				keep("secret", secret)
				keep("stated", "1")
			}
			if err := change(state, "", []edit.KV{
				{Path: "actModeApiProvider", Value: "openai"}, {Path: "planModeApiProvider", Value: "openai"},
				{Path: "actModeOpenAiModelId", Value: ref}, {Path: "planModeOpenAiModelId", Value: ref},
				{Path: "openAiBaseUrl", Value: v1},
				{Path: "openAiHeaders", Value: map[string]string{"User-Agent": "cline"}},
			}, []string{"actModeOpenAiModelInfo", "planModeOpenAiModelInfo"}); err != nil {
				return plan, err
			}
			if err := change(secrets, "", []edit.KV{{Path: secretKey, Value: pick.Gateway.Key()}}, nil); err != nil {
				return plan, err
			}
		}
		effort := Effort(config.Get("providers." + own + ".settings.reasoning").String())
		err := change(providers, EmptyProviders, []edit.KV{
			{Path: slot, Value: providerEntry(ref, effort, v1, pick.Gateway.Key())},
			{Path: lastUsed, Value: ProviderSlot},
		}, nil)
		return plan, err
	}
	providerBody := providers.Body
	if onMagpie {
		if saved("stated") != "" || stateOnMagpie {
			if state.Exists {
				var original map[string]json.RawMessage
				if snapshot := saved("state"); snapshot != "" {
					if err := json.Unmarshal([]byte(snapshot), &original); err != nil {
						return plan, edit.AtFile(state.Path, err)
					}
				}
				var set []edit.KV
				var del []string
				for _, key := range stateKeys {
					if value, ok := original[key]; ok {
						set = append(set, edit.KV{Path: key, Value: value})
					} else {
						del = append(del, key)
					}
				}
				if err := change(state, "", set, del); err != nil {
					return plan, err
				}
			}
			if secrets.Exists {
				original := saved("secret")
				if err := restore(secrets, secretKey, original, original); err != nil {
					return plan, err
				}
			}
		}
		originalCatalog := saved("models")
		if err := restore(models, slot, originalCatalog, json.RawMessage(originalCatalog)); err != nil {
			return plan, err
		}
		last = saved(lastUsed)
		var set []edit.KV
		var del []string
		if entry := saved("entry"); entry != "" {
			set = append(set, edit.KV{Path: slot, Value: json.RawMessage(entry)})
		} else {
			del = append(del, slot)
		}
		if last != "" {
			set = append(set, edit.KV{Path: lastUsed, Value: last})
		} else {
			del = append(del, lastUsed)
		}
		var err error
		providerBody, err = patch(providers, providerBody, set, del)
		if err != nil {
			return plan, err
		}
		pick.Restoring = true
	}
	if pick.Model != "" {
		own = last
		if own == "" {
			own = "cline"
		}
		if !providers.Exists {
			providerBody = []byte(EmptyProviders + "\n")
		}
		prefix := "providers." + own + ".settings."
		var err error
		providerBody, err = patch(providers, providerBody, []edit.KV{
			{Path: prefix + "provider", Value: own}, {Path: prefix + "model", Value: pick.Model},
		}, nil)
		if err != nil {
			return plan, err
		}
	} else if !onMagpie && last != "" {
		if err := change(providers, "", nil, []string{"providers." + last + ".settings.model"}); err != nil {
			return plan, err
		}
		return plan, nil
	}
	add(providers, providerBody)
	return plan, nil
}
