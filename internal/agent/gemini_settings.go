package agent

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

const (
	geminiBaseKey  = "GOOGLE_GEMINI_BASE_URL"
	geminiAPIKey   = "GEMINI_API_KEY"
	geminiAuthPath = "security.auth.selectedType"
	geminiModel    = "model.name"
)

type geminiChoice struct {
	model, auth, apiKey string
	reset               bool
}

func geminiRouted(base, key, gateway string) bool {
	return base == gateway || base != "" && (ourKey(key) || key == keyAt(gateway))
}

func changeGeminiSettings(at place, path, envPath string, choice geminiChoice) error {
	env, err := readWiringFile(envPath)
	if err != nil {
		return edit.PreserveOnRollback(envPath, err)
	}
	base, _ := edit.GetEnv(env.body, geminiBaseKey)
	key, _ := edit.GetEnv(env.body, geminiAPIKey)
	gateway := at.gw()
	routed := geminiRouted(base, key, gateway)
	if !routed && choice == (geminiChoice{}) {
		return env.check(envPath)
	}
	baseSaved, keySaved := at.key("gemini.base_url"), at.key("gemini.api_key")
	authSaved, modelSaved := at.key("gemini.auth"), at.key("gemini.model")
	saved := peek(baseSaved, keySaved, authSaved, modelSaved)
	nextBase, nextKey := base, key
	var nextAuth, nextModel string
	if routed && !choice.reset {
		nextBase, nextKey = saved[baseSaved], saved[keySaved]
		nextAuth, nextModel = saved[authSaved], saved[modelSaved]
	} else if routed {
		nextBase, nextKey = "", ""
	}
	nativeAuth := choice.auth != "" && choice.auth != "custom"
	if choice.auth == "custom" {
		if nextBase == "" || geminiRouted(nextBase, nextKey, gateway) {
			return fmt.Errorf("custom means whatever GOOGLE_GEMINI_BASE_URL is already in %s; set it there", envPath)
		}
		if !routed {
			return env.check(envPath)
		}
	}
	config, err := readWiringFile(path)
	if err != nil {
		return edit.PreserveOnRollback(path, err)
	}
	if config.target == env.target || config.info != nil && env.info != nil && os.SameFile(config.info, env.info) {
		err := fmt.Errorf("Gemini settings and environment alias the same file: %w", extension.ErrActionChanged)
		return edit.PreserveOnRollback(envPath, edit.PreserveOnRollback(path, err))
	}

	nextEnv := env.body
	route := isMagpie(choice.model)
	if route {
		nextEnv = edit.PatchEnv(env.body, edit.KV{Path: geminiBaseKey, Value: gateway}, edit.KV{Path: geminiAPIKey, Value: keyAt(gateway)})
		nextAuth = "gemini-api-key"
	} else {
		if nativeAuth {
			nextBase, nextAuth = "", choice.auth
			if choice.auth == "default" {
				nextAuth = ""
			}
			if choice.apiKey != "" {
				nextKey = choice.apiKey
			}
		}
		nextEnv = geminiNativeEnv(env.body, routed, nativeAuth, nextBase, nextKey, key)
	}
	if choice.model != "" {
		nextModel = choice.model
	}
	var put []edit.KV
	var drop []string
	set := func(path, value string) {
		if value == "" {
			drop = append(drop, path)
		} else {
			put = append(put, edit.KV{Path: path, Value: value})
		}
	}
	if route || routed || nativeAuth {
		set(geminiAuthPath, nextAuth)
	}
	modelChanged := routed || choice.model != "" || choice.reset
	if modelChanged {
		set(geminiModel, nextModel)
	}
	nextConfig := config.body
	if len(put) > 0 || len(drop) > 0 && len(bytes.TrimSpace(edit.TrimBOM(config.body))) > 0 {
		nextConfig, err = edit.PatchJSON(config.body, put, drop)
		if err == nil && modelChanged && nextModel == "" {
			model := gjson.GetBytes(jsonc.ToJSON(edit.TrimBOM(nextConfig)), "model")
			if model.IsObject() && len(model.Map()) == 0 {
				nextConfig, err = edit.PatchJSON(nextConfig, nil, []string{"model"})
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := publishGeminiSettings(
		geminiFileEdit{path: path, before: config, body: nextConfig},
		geminiFileEdit{path: envPath, before: env, body: nextEnv},
	); err != nil {
		return err
	}
	if route && !routed {
		settings := gjson.ParseBytes(jsonc.ToJSON(edit.TrimBOM(config.body)))
		stash(map[string]string{
			baseSaved: base, keySaved: key, authSaved: settings.Get(geminiAuthPath).String(), modelSaved: settings.Get(geminiModel).String(),
		})
	} else if routed && !route {
		forgetValues(saved)
	}
	return nil
}

func geminiNativeEnv(body []byte, routed, nativeAuth bool, base, key, currentKey string) []byte {
	var set []edit.KV
	if routed {
		body, _ = edit.PatchEnvWithout(body, geminiBaseKey, geminiAPIKey)
		if base != "" {
			set = append(set, edit.KV{Path: geminiBaseKey, Value: base})
		}
		if key != "" {
			set = append(set, edit.KV{Path: geminiAPIKey, Value: key})
		}
	} else if nativeAuth {
		body, _ = edit.PatchEnvWithout(body, geminiBaseKey)
		if key != currentKey {
			set = append(set, edit.KV{Path: geminiAPIKey, Value: key})
		}
	}
	if len(set) > 0 {
		body = edit.PatchEnv(body, set...)
	}
	return body
}

type geminiFileEdit struct {
	path   string
	before wiringFile
	body   []byte
}

func publishGeminiSettings(config, env geminiFileEdit) (err error) {
	files := [...]geminiFileEdit{config, env}
	current := [...]wiringFile{config.before, env.before}
	check := func() error { return errors.Join(current[0].check(config.path), current[1].check(env.path)) }
	var published [len(files)]bool
	defer func() {
		if err == nil {
			return
		}
		err = errors.Join(err, check())
		for index := len(files) - 1; index >= 0; index-- {
			if !published[index] {
				continue
			}
			change, written := files[index], current[index]
			verify := func() error { return written.check(change.path) }
			var restoreErr error
			if change.before.info == nil {
				restoreErr = edit.RemoveChecked(written.target, verify, nil)
			} else {
				restoreErr = edit.WriteAtomicChecked(written.target, change.before.body, verify)
			}
			if restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore Gemini settings %s: %w", change.path, restoreErr))
			}
		}
		for _, change := range files {
			err = edit.PreserveOnRollback(change.path, err)
		}
	}()
	if err := check(); err != nil {
		return err
	}
	for index, change := range files {
		if bytes.Equal(edit.TrimBOM(change.before.body), edit.TrimBOM(change.body)) {
			continue
		}
		var write edit.AtomicPublication
		if change.body == nil {
			err = edit.RemoveChecked(change.path, check, &write)
		} else {
			err = edit.WriteAtomicRecording(change.before.target, change.body, check, &write)
		}
		if write.Info != nil {
			body := write.Body
			if write.RetainedTail && len(body) < len(change.before.body) {
				body = append(bytes.Clone(body), change.before.body[len(body):]...)
			}
			current[index] = wiringFile{target: change.before.target, info: write.Info, body: body}
			published[index] = true
		}
		if err != nil {
			return err
		}
	}
	return nil
}
