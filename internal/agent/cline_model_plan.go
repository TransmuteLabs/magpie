package agent

import (
	"bytes"
	"encoding/json"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type clineModelChoice struct {
	at        place
	model     string
	prefix    string
	saved     map[string]string
	keep      map[string]string
	restoring bool
}

func clineSelectModel(at place, model string, files []extension.LinkedActionFile) error {
	pick := clineModelChoice{
		at: at, model: model, prefix: "cline:" + files[0].Path + ":",
		keep: make(map[string]string),
	}
	pick.saved = peek(pick.prefix+"entry", pick.prefix+"lastUsedProvider", pick.prefix+"models",
		pick.prefix+"state", pick.prefix+"secret", pick.prefix+"stated")
	if err := extension.ApplyLinkedAgentPlan(&pick, clineModelPlan, files...); err != nil {
		return err
	}
	if len(pick.keep) != 0 {
		stash(pick.keep)
	}
	if pick.restoring {
		forgetValues(pick.saved)
	}
	return nil
}

func clineModelPlan(pick *clineModelChoice, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
	var plan sdk.AgentPlan
	providers, state, secrets, models := settings.Files[0], settings.Files[1], settings.Files[2], settings.Files[3]
	const slot = "providers." + clineSlot
	const lastUsed = "lastUsedProvider"
	const secretKey = "openAiApiKey"
	config := clineReadSettings(providers)
	last := config.Get(lastUsed).String()
	onMagpie := last == clineSlot && ourKey(config.Get(slot+".settings.apiKey").String())
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
	saved := func(key string) string { return pick.saved[pick.prefix+key] }
	keep := func(key, value string) { pick.keep[pick.prefix+key] = value }
	secret := clineReadSettings(secrets).Get(secretKey).String()
	stateConfig := clineReadSettings(state)
	v1 := pick.at.v1()
	stateOnMagpie := ourKey(secret) && stateConfig.Get("openAiBaseUrl").String() == v1
	if ref, ok := cutMagpie(pick.model); ok {
		catalog := clineReadSettings(models).Get(slot).String()
		if !onMagpie {
			keep("entry", config.Get(slot).String())
			keep(lastUsed, last)
			keep("models", catalog)
		}
		if err := change(models, `{"version":1,"providers":{}}`, []edit.KV{
			{Path: slot, Value: clineKeptValue(catalog, clineModelsAt(ref, v1))},
		}, nil); err != nil {
			return plan, err
		}
		if state.Exists {
			if !stateOnMagpie {
				snapshot := make(map[string]json.RawMessage)
				for _, key := range clineStateKeys {
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
			if err := change(secrets, "", []edit.KV{{Path: secretKey, Value: pick.at.gwKey()}}, nil); err != nil {
				return plan, err
			}
		}
		effort := clineEffort(config.Get("providers." + own + ".settings.reasoning").String())
		err := change(providers, providersEmpty, []edit.KV{
			{Path: slot, Value: clineProvider(ref, effort, pick.at)},
			{Path: lastUsed, Value: clineSlot},
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
				for _, key := range clineStateKeys {
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
		pick.restoring = true
	}
	if pick.model != "" {
		own = last
		if own == "" {
			own = "cline"
		}
		if !providers.Exists {
			providerBody = []byte(providersEmpty + "\n")
		}
		prefix := "providers." + own + ".settings."
		var err error
		providerBody, err = patch(providers, providerBody, []edit.KV{
			{Path: prefix + "provider", Value: own}, {Path: prefix + "model", Value: pick.model},
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
