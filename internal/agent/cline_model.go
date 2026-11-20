package agent

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/edit"
)

func readClineFiles(files ...*wiringFileEdit) error {
	for _, file := range files {
		before, err := readWiringFile(file.path)
		if err != nil {
			return edit.PreserveOnRollback(file.path, err)
		}
		file.before = before
	}
	return nil
}

func clineReadSettings(file wiringFileEdit) gjson.Result {
	return gjson.ParseBytes(jsonc.ToJSON(edit.TrimBOM(file.before.body)))
}

func patchClineFile(file *wiringFileEdit, seed string, set []edit.KV, del []string) error {
	body := file.body
	if body == nil {
		body = file.before.body
	}
	if len(set) == 0 && (len(del) == 0 || len(bytes.TrimSpace(edit.TrimBOM(body))) == 0) {
		return nil
	}
	if file.before.info == nil && file.body == nil && seed != "" {
		body = []byte(seed + "\n")
	}
	changed, err := edit.PatchJSON(body, set, del)
	if err != nil {
		return file.before.preserveOnError(file.path, fmt.Errorf("%s: %w", file.path, err))
	}
	file.body = nil
	if !bytes.Equal(file.before.body, changed) {
		file.body = changed
	}
	return nil
}

func restoreClineValue(file *wiringFileEdit, key, saved string, value any) error {
	if saved == "" {
		return patchClineFile(file, "", nil, []string{key})
	}
	return patchClineFile(file, "", []edit.KV{{Path: key, Value: value}}, nil)
}

func clineSelectModel(at place, value, providersPath, statePath, secretsPath, modelsPath string) error {
	const slot = "providers." + clineSlot
	const lastUsed = "lastUsedProvider"
	const secretKey = "openAiApiKey"
	providers := wiringFileEdit{path: providersPath, private: true}
	if err := readClineFiles(&providers); err != nil {
		return err
	}
	config := clineReadSettings(providers)
	last := config.Get(lastUsed).String()
	onMagpie := last == clineSlot && ourKey(config.Get(slot+".settings.apiKey").String())
	ref, magpie := cutMagpie(value)
	if !onMagpie && !magpie {
		if err := clinePickOwnModel(&providers, last, value); err != nil {
			return err
		}
		return writeWiringFiles(providers)
	}

	models := wiringFileEdit{path: modelsPath, private: true}
	state := wiringFileEdit{path: statePath, private: true}
	secrets := wiringFileEdit{path: secretsPath, private: true}
	if err := readClineFiles(&models, &state, &secrets); err != nil {
		return err
	}
	key := "cline:" + providers.path + ":"
	v1 := at.v1()
	stateConfig := clineReadSettings(state)
	secret := clineReadSettings(secrets).Get(secretKey).String()
	stateOnMagpie := ourKey(secret) && stateConfig.Get("openAiBaseUrl").String() == v1
	if magpie {
		originals := map[string]string{}
		catalog := clineReadSettings(models).Get(slot).String()
		if !onMagpie {
			originals[key+"entry"] = config.Get(slot).String()
			originals[key+lastUsed] = last
			originals[key+"models"] = catalog
		}
		if err := patchClineFile(&models, `{"version":1,"providers":{}}`, []edit.KV{
			{Path: slot, Value: clineKeptValue(catalog, clineModelsAt(ref, v1))},
		}, nil); err != nil {
			return err
		}
		if state.before.info != nil {
			if !stateOnMagpie {
				snapshot := make(map[string]json.RawMessage)
				for _, field := range clineStateKeys {
					if old := stateConfig.Get(field); old.Exists() {
						snapshot[field] = json.RawMessage(old.Raw)
					}
				}
				raw, err := json.Marshal(snapshot)
				if err != nil {
					return state.before.preserveOnError(state.path, fmt.Errorf("%s: %w", state.path, err))
				}
				originals[key+"state"], originals[key+"secret"], originals[key+"stated"] = string(raw), secret, "1"
			}
			if err := patchClineFile(&state, "", []edit.KV{
				{Path: "actModeApiProvider", Value: "openai"}, {Path: "planModeApiProvider", Value: "openai"},
				{Path: "actModeOpenAiModelId", Value: ref}, {Path: "planModeOpenAiModelId", Value: ref},
				{Path: "openAiBaseUrl", Value: v1},
				{Path: "openAiHeaders", Value: map[string]string{"User-Agent": "cline"}},
			}, []string{"actModeOpenAiModelInfo", "planModeOpenAiModelInfo"}); err != nil {
				return err
			}
			if err := patchClineFile(&secrets, "", []edit.KV{{Path: secretKey, Value: at.gwKey()}}, nil); err != nil {
				return err
			}
		}
		own := last
		if own == "" {
			own = "cline"
		}
		effort := clineEffort(config.Get("providers." + own + ".settings.reasoning").String())
		if err := patchClineFile(&providers, providersEmpty, []edit.KV{
			{Path: slot, Value: clineProvider(ref, effort, at)},
			{Path: lastUsed, Value: clineSlot},
		}, nil); err != nil {
			return err
		}
		if err := writeWiringFiles(models, state, secrets, providers); err != nil {
			return err
		}
		if len(originals) != 0 {
			stash(originals)
		}
		return nil
	}

	saved := peek(key+"entry", key+lastUsed, key+"models", key+"state", key+"secret", key+"stated")
	if saved[key+"stated"] != "" || stateOnMagpie {
		if state.before.info != nil {
			if err := restoreClineState(&state, saved[key+"state"]); err != nil {
				return err
			}
		}
		if secrets.before.info != nil {
			original := saved[key+"secret"]
			if err := restoreClineValue(&secrets, secretKey, original, original); err != nil {
				return err
			}
		}
	}
	catalog := saved[key+"models"]
	if err := restoreClineValue(&models, slot, catalog, json.RawMessage(catalog)); err != nil {
		return err
	}
	entry := saved[key+"entry"]
	if err := restoreClineValue(&providers, slot, entry, json.RawMessage(entry)); err != nil {
		return err
	}
	last = saved[key+lastUsed]
	if err := restoreClineValue(&providers, lastUsed, last, last); err != nil {
		return err
	}
	if value != "" {
		if err := clinePickOwnModel(&providers, last, value); err != nil {
			return err
		}
	}
	if err := writeWiringFiles(models, state, secrets, providers); err != nil {
		return err
	}
	forgetValues(saved)
	return nil
}

func clinePickOwnModel(providers *wiringFileEdit, last, value string) error {
	if value == "" {
		if last == "" {
			return nil
		}
		return patchClineFile(providers, "", nil, []string{"providers." + last + ".settings.model"})
	}
	if last == "" {
		last = "cline"
	}
	prefix := "providers." + last + ".settings."
	return patchClineFile(providers, providersEmpty, []edit.KV{
		{Path: prefix + "provider", Value: last}, {Path: prefix + "model", Value: value},
	}, nil)
}

func restoreClineState(file *wiringFileEdit, snapshot string) error {
	var original map[string]json.RawMessage
	if snapshot != "" {
		if err := json.Unmarshal([]byte(snapshot), &original); err != nil {
			return file.before.preserveOnError(file.path, fmt.Errorf("%s: %w", file.path, err))
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
	return patchClineFile(file, "", set, del)
}
