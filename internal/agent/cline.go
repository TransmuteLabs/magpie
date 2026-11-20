package agent

// Cline's CLI (3.x) keeps its providers in $CLINE_DIR/data/settings/
// providers.json, ~/.cline by default:
//
//	{"version":1,"lastUsedProvider":"<id>","modes":{},"providers":{"<id>":{
//	  "settings":{"provider":"<id>","apiKey":…,"model":…,"baseUrl":…,
//	    "headers":{…},"reasoning":{"effort":…}},
//	  "updatedAt":…,"tokenSource":"manual"}}}
//
// A session runs the model of lastUsedProvider, asking for its
// reasoning.effort. A provider of one's own, added to models.json, is
// refused when a session starts ("Unknown or disabled provider",
// cline/cline#14180), so magpie takes Cline's built-in openai-compatible
// provider, which reads the models from the gateway's /v1/models, and makes
// it the one in use. That provider lists only gpt-4o, so magpie's models
// are put in models.json beside it, as the entry Cline's own migration writes
// for it ({"provider":{"name","baseUrl","defaultModelId"},"models":{…}}),
// which takes the place of the built-in list. What was there, and the
// provider in use, are stashed and put back when magpie steps out. Cline's
// requests name only the AI SDK, so the header says they are Cline's.
//
// reasoning is {"enabled":false} for no thinking, as Cline's own pickers
// write it; an effort beside a false enabled is dropped, so magpie writes
// {"enabled":true,"effort":…} for an effort.
//
// Cline's VS Code extension (4.x) runs on the same files, but takes the
// provider, model, base URL and key from its own state beside them first —
// $CLINE_DATA_DIR (else $CLINE_DIR/data) globalState.json's
// act/planModeApiProvider ("openai" for openai-compatible),
// act/planModeOpenAiModelId and openAiBaseUrl, secrets.json's openAiApiKey
// — and providers.json's lastUsedProvider only when no mode has one. Where
// that state is, magpie sets those too, stashing what they were. The
// extension reads the files once, at start-up.
//
// Cline's desktop app (0.0.x) keeps the provider, model and effort picked
// in its composer in its web view's localStorage, per provider, and never
// reads them from these files: magpie's models are in its picker, under
// magpie, but which one it runs is picked there.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/edit"
)

// clineSlot is the provider magpie takes in Cline.
const clineSlot = "openai-compatible"

func cline(home string) *Agent { return clineIn(here(home)) }

// clineIn is Cline at a place: this machine's home, or a WSL distro's (see
// wsl.go), where CLINE_DIR and CLINE_DATA_DIR aren't read and its provider
// names the gateway as the distro reaches it, with the key it takes from
// there.
func clineIn(at place) *Agent {
	dir := at.getenv("CLINE_DIR")
	if dir == "" {
		dir = at.join(".cline")
	}
	data := at.getenv("CLINE_DATA_DIR")
	if data == "" {
		data = filepath.Join(dir, "data")
	}
	path := filepath.Join(data, "settings", "providers.json")
	models := filepath.Join(data, "settings", "models.json")
	state, secrets := filepath.Join(data, "globalState.json"), filepath.Join(data, "secrets.json")
	slot := "providers." + clineSlot
	get := func(k string) string { v, _ := edit.GetJSON(path, k); return v }
	inUse := func() string { return get("lastUsedProvider") }
	onMagpie := func() bool {
		return inUse() == clineSlot && ourKey(get(slot+".settings.apiKey"))
	}
	// own is the provider a model or effort of Cline's own is set on: the
	// one in use, else Cline's default
	own := func() string {
		if p := inUse(); p != "" {
			return p
		}
		return "cline"
	}
	// the setting its CLI reads for every telemetry event; its env path wins
	settingsPath := at.getenv("CLINE_GLOBAL_SETTINGS_PATH")
	if settingsPath == "" {
		settingsPath = filepath.Join(data, "settings", "global-settings.json")
	}
	return &Agent{
		Telemetry: clineTelemetry(at, settingsPath),
		ID:        "cline", Name: "Cline", Icon: "cline", Aliases: []string{"cline-cli"}, Spelled: prefixed,
		UA:  []string{"cline"},
		Bin: "cline", Dir: dir, Path: path,
		Sync: func() error {
			return clineSync(at, path, state, secrets, models)
		},
		Notice: func() string {
			if runtime.GOOS == "windows" {
				// Running can't tell the desktop app from the CLI there
				return noticeCline1.String()
			}
			if Running(`Cline\.app/`, `(^|/)cline-app( |$)`) {
				return noticeCline2.String()
			}
			if Running(`(^|/)cline( |$)`) {
				return noticeCline3.String()
			}
			return ""
		},
		Check: func() string {
			if !onMagpie() {
				return ""
			}
			return wiringOff("Cline", path, func(k string) (string, bool) { return edit.GetJSON(path, slot+".settings."+k) },
				"baseUrl", at.v1())
		},
		Fields: []Field{{
			Key: "model", Label: "model",
			Get: func() string {
				p := inUse()
				if p == "" {
					return ""
				}
				v := get("providers." + p + ".settings.model")
				if v != "" && onMagpie() {
					return magpieID + "/" + v
				}
				return v
			},
			Set: func(v string) error {
				return clineSelectModel(at, v, path, state, secrets, models)
			},
			Options: func(cur map[string]string) []Option {
				return withVia(ownOptions("", cur["model"]), "cline", magpieID+"/")
			},
		}, {
			// the reasoning of the provider in use, what Cline's --thinking
			// (and its extension) asks for when it is not given
			Key: "effort", Label: "effort",
			Get: func() string {
				p := inUse()
				if p == "" {
					return ""
				}
				return clineEffort(get("providers." + p + ".settings.reasoning"))
			},
			Set: func(v string) error {
				k := "providers." + own() + ".settings.reasoning"
				if v == "" {
					return edit.DelJSON(path, k)
				}
				return clineWrite(path, providersEmpty, edit.KV{Path: k, Value: clineReasoning(v)})
			},
			Options: func(map[string]string) []Option {
				return static("none", "low", "medium", "high", "xhigh")
			},
		}},
	}
}

func clineSync(at place, providersPath, statePath, secretsPath, modelsPath string) error {
	providers := wiringFileEdit{path: providersPath}
	state := wiringFileEdit{path: statePath}
	secrets := wiringFileEdit{path: secretsPath}
	models := wiringFileEdit{path: modelsPath}
	if err := readClineFiles(&providers, &state, &secrets, &models); err != nil {
		return err
	}
	const slot = "providers." + clineSlot
	const prefix = slot + ".settings."
	config := clineReadSettings(providers)
	if config.Get("lastUsedProvider").String() != clineSlot || !ourKey(config.Get(prefix+"apiKey").String()) {
		return nil
	}
	update := func(file *wiringFileEdit, key string, current gjson.Result, value any) error {
		if !current.Exists() || sameJSON(current.Raw, value) {
			return nil
		}
		body, err := edit.PatchJSON(file.before.body, []edit.KV{{Path: key, Value: value}}, nil)
		if err != nil {
			return file.before.preserveOnError(file.path, fmt.Errorf("%s: %w", file.path, err))
		}
		file.body = body
		return nil
	}
	v1 := at.v1()
	if err := update(&providers, prefix+"baseUrl", config.Get(prefix+"baseUrl"), v1); err != nil {
		return err
	}
	if ourKey(clineReadSettings(secrets).Get("openAiApiKey").String()) {
		const stateURL = "openAiBaseUrl"
		if err := update(&state, stateURL, clineReadSettings(state).Get(stateURL), v1); err != nil {
			return err
		}
	}
	if current := clineReadSettings(models).Get(slot); current.Exists() {
		want := clineModelsAt(config.Get(prefix+"model").String(), v1)
		if err := update(&models, slot, current, clineKeptValue(current.String(), want)); err != nil {
			return err
		}
	}
	return writeWiringFiles(providers, state, secrets, models)
}

// cutMagpie is the model a magpie/<ref> value names, when it is magpie's.
func cutMagpie(v string) (string, bool) {
	ref, ok := strings.CutPrefix(v, magpieID+"/")
	return ref, ok && isMagpie(ref)
}

// clineProvider is magpie's openai-compatible entry, asking model with
// effort (none unset), for the Cline at a place.
func clineProvider(model, effort string, at place) map[string]any {
	s := map[string]any{
		"provider": clineSlot, "apiKey": at.gwKey(), "model": model, "baseUrl": at.v1(),
		"headers": map[string]string{"User-Agent": "cline"},
	}
	if effort != "" {
		s["reasoning"] = clineReasoning(effort)
	}
	return map[string]any{"settings": s, "updatedAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), "tokenSource": "manual"}
}

// clineReasoning is a provider's reasoning asking for effort, none for no
// thinking.
func clineReasoning(effort string) map[string]any {
	if effort == "none" {
		return map[string]any{"enabled": false}
	}
	return map[string]any{"enabled": true, "effort": effort}
}

// clineEffort is the effort a provider's reasoning asks for: none when it
// is off, whatever effort is left beside that.
func clineEffort(raw string) string {
	var r struct {
		Enabled *bool  `json:"enabled"`
		Effort  string `json:"effort"`
	}
	if json.Unmarshal([]byte(raw), &r) != nil {
		return ""
	}
	if r.Enabled != nil && !*r.Enabled {
		return "none"
	}
	return r.Effort
}

// clineStateKeys are the keys of the extension's globalState.json magpie
// sets.
var clineStateKeys = []string{"actModeApiProvider", "planModeApiProvider", "actModeOpenAiModelId", "planModeOpenAiModelId",
	"actModeOpenAiModelInfo", "planModeOpenAiModelInfo", "openAiBaseUrl", "openAiHeaders"}

// clineModels is magpie's entry in models.json: every magpie model, model
// the one the provider starts on.
func clineModels(model string) map[string]any { return clineModelsAt(model, gatewayV1()) }

// clineModelsAt is clineModels for a Cline reaching the gateway's /v1 at v1.
func clineModelsAt(model, v1 string) map[string]any {
	ms := map[string]any{}
	for _, m := range sharedModels("cline") {
		caps := []string{"streaming", "tools"}
		if m.Images {
			caps = append(caps, "images")
		}
		if len(m.Efforts) > 0 {
			caps = append(caps, "reasoning")
		}
		e := map[string]any{"id": m.ID, "name": m.Name, "capabilities": caps}
		if m.Context > 0 {
			e["contextWindow"] = m.Context
		}
		if m.Output > 0 {
			e["maxTokens"] = maxTokens(m)
		}
		ms[m.ID] = e
	}
	return map[string]any{
		"provider": map[string]any{"name": "magpie", "baseUrl": v1, "defaultModelId": model},
		"models":   ms,
	}
}

// clineKeptValue is magpie's entry want laid over the one in Cline's models.json.
// What the user set there stays (#2030): a key of the provider's
// beside its name, address and default model (protocol, client,
// capabilities, modelsSourceUrl), and on each of magpie's models what
// magpie doesn't write, such as an override from the VS Code extension's
// model editor (temperature, prices, apiFormat, supportsVision;
// sdk/packages/core/src/services/providers/local-provider-registry.ts). A
// model magpie no longer lists goes, with what was set on it.
func clineKeptValue(cur string, want map[string]any) any {
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
		ms[id] = mergeKept(h, w.(map[string]any), nil)
	}
	out := mergeKept(have, want, []string{"models"})
	out["models"] = ms
	return out
}

// providersEmpty is a providers.json with nothing in it.
const providersEmpty = `{"version":1,"modes":{},"providers":{}}`

// clineWrite sets kvs in a file of Cline's settings, which it keeps
// private, made from empty when there is none.
func clineWrite(path, empty string, kvs ...edit.KV) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(empty+"\n"), 0o600); err != nil {
			return err
		}
	}
	return edit.SetJSON(path, kvs...)
}

// what cline says after a change (notice.go)
var (
	noticeCline1 = newNotice("Cline reads its provider as a session starts — open sessions keep the model they have; new ones use this. Reload VS Code's window for its extension; Cline's desktop app keeps the model picked in its own composer — pick magpie's there.")
	noticeCline2 = newNotice("Cline's desktop app keeps the model and effort picked in its own composer — pick magpie's there; this sets Cline's CLI and VS Code extension.")
	noticeCline3 = newNotice("Cline reads its provider as a session starts — open sessions keep the model they have; new ones use this. Reload VS Code's window for its extension.")
)
