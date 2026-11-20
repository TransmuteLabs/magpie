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
	"path/filepath"
	"runtime"
	"strings"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtincline "github.com/yetone/magpie/internal/extension/builtin/cline"
)

// clineSlot is the provider magpie takes in Cline.
const clineSlot = builtincline.ProviderSlot

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
	files := []extension.LinkedActionFile{
		{Name: builtincline.ProvidersFile, Path: path, Private: true},
		{Name: builtincline.StateFile, Path: state, Private: true},
		{Name: builtincline.SecretsFile, Path: secrets, Private: true},
		{Name: builtincline.ModelsFile, Path: models, Private: true},
	}
	slot := "providers." + clineSlot
	get := func(k string) string { v, _ := edit.GetJSON(path, k); return v }
	inUse := func() string { return get("lastUsedProvider") }
	onMagpie := func() bool {
		return inUse() == clineSlot && ourKey(get(slot+".settings.apiKey"))
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
			return extension.ApplyLinkedAgentPlan(clineGateway(at), builtincline.PlanSync, files...)
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
				return clineSelectModel(at, v, files)
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
				return builtincline.Effort(get("providers." + p + ".settings.reasoning"))
			},
			Set: func(v string) error {
				return extension.ApplyLinkedAgentPlan(v, builtincline.PlanEffort, files[0])
			},
			Options: func(map[string]string) []Option {
				return static("none", "low", "medium", "high", "xhigh")
			},
		}},
	}
}

// cutMagpie is the model a magpie/<ref> value names, when it is magpie's.
func cutMagpie(v string) (string, bool) {
	ref, ok := strings.CutPrefix(v, magpieID+"/")
	return ref, ok && isMagpie(ref)
}

func clineGateway(at place) builtincline.GatewaySettings {
	return builtincline.GatewaySettings{URL: at.v1, Key: at.gwKey, OwnKey: ourKey, Models: clineCatalogModels}
}

func clineCatalogModels() []builtincline.Model {
	models := sharedModels("cline")
	out := make([]builtincline.Model, len(models))
	for index, model := range models {
		out[index] = builtincline.Model{
			ID: model.ID, Name: model.Name, Context: model.Context, Output: maxTokens(model),
			Images: model.Images, Reasoning: len(model.Efforts) > 0,
		}
	}
	return out
}

// what cline says after a change (notice.go)
var (
	noticeCline1 = newNotice("Cline reads its provider as a session starts — open sessions keep the model they have; new ones use this. Reload VS Code's window for its extension; Cline's desktop app keeps the model picked in its own composer — pick magpie's there.")
	noticeCline2 = newNotice("Cline's desktop app keeps the model and effort picked in its own composer — pick magpie's there; this sets Cline's CLI and VS Code extension.")
	noticeCline3 = newNotice("Cline reads its provider as a session starts — open sessions keep the model they have; new ones use this. Reload VS Code's window for its extension.")
)
