package agent

// Qoder's CLI (1.1) keeps its settings in $QODER_CONFIG_DIR/settings.json,
// ~/.qoder by default. A provider of one's own is an entry of "providers",
// its models picked as "<provider>/<model>":
//
//	{"providers":{"magpie":{"displayName":"magpie","protocol":"openai",
//	   "baseUrl":"http://127.0.0.1:3425/v1","apiKey":"magpie","model":…,
//	   "models":[{"model":…,"displayName":…,"contextWindow":…,"maxOutputTokens":…,
//	     "capabilities":{"tools":true,"vision":…,"thinking":{"modes":["enabled"],
//	       "supportsEffort":true,"supportedEffortLevels":[…]}}}]}},
//	 "model":{"name":"magpie/<model>","reasoningEffort":…,
//	   "preferences":{"magpie/<model>":{"reasoning":{"effort":…}}}}}
//
// Each model's effort is its own, in model.preferences (1.1.6x); the one in
// model.reasoningEffort is asked for only by a model with none there, and
// Qoder moves it into the preferences of the model it starts on, and drops
// it, so it counts once. magpie sets the model's preference, and keeps
// reasoningEffort for the Qoders before preferences.
//
// An openai provider is asked at baseUrl + /chat/completions, with
// reasoning_effort when the model says it takes one. Qoder offers custom
// providers only to a signed-in account whose plan has BYOK. Its requests
// say only undici, so they are not told apart from other clients'.
//
// Qoder CN (qoderclicn, @qodercn-ai/qoderclicn) is the same CLI for the
// China site, its own accounts and settings in $QODERCN_CONFIG_DIR,
// ~/.qoder-cn by default.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/yetone/magpie/internal/edit"
	builtinqoder "github.com/yetone/magpie/internal/extension/builtin/qoder"
	"github.com/yetone/magpie/internal/gateway"
)

func qoder(home string) *Agent {
	return qoderSite(home, qoderGlobal)
}

func qoderCN(home string) *Agent {
	return qoderSite(home, qoderChina)
}

// qoderBuild is what tells Qoder's two builds apart.
type qoderBuild struct {
	id, name, env, dir, bin string
	aliases, procs          []string
}

var (
	qoderGlobal = qoderBuild{id: "qoder", name: "Qoder", env: "QODER_CONFIG_DIR", dir: ".qoder", bin: "qodercli",
		aliases: []string{"qodercli", "qoder-cli"}, procs: []string{`(^|/)qodercli( |$)`, `(^|/)qoder( |$)`}}
	qoderChina = qoderBuild{id: "qoder-cn", name: "Qoder CN", env: "QODERCN_CONFIG_DIR", dir: ".qoder-cn", bin: "qoderclicn",
		aliases: []string{"qoderclicn", "qodercn", "qodercn-cli"}, procs: []string{`(^|/)qoderclicn( |$)`, `(^|/)qodercn( |$)`}}
)

// qoderIn and qoderCNIn are Qoder's two builds in a WSL distro (see
// wsl.go): at their default folders, as the distro's variables aren't read.
func qoderIn(at place) *Agent   { return qoderAt(at, qoderGlobal) }
func qoderCNIn(at place) *Agent { return qoderAt(at, qoderChina) }

func qoderSite(home string, b qoderBuild) *Agent { return qoderAt(here(home), b) }

// qoderAt is a build of Qoder at a place, its provider naming the gateway
// as it reaches it from there.
func qoderAt(at place, b qoderBuild) *Agent {
	dir := at.getenv(b.env)
	if dir == "" {
		dir = at.join(b.dir)
	}
	path := filepath.Join(dir, builtinqoder.SettingsFile)
	key := b.id + ":" + path + ":model"
	const slot = builtinqoder.ProviderPath
	get := func(k string) string { v, _ := edit.GetJSON(path, k); return v }
	effort := func() string {
		return builtinqoder.Effort(get("model.preferences"), get(builtinqoder.ModelPath), get("model.reasoningEffort"))
	}
	apply := func(action builtinqoder.Action, value string) error {
		var saved map[string]string
		if action == builtinqoder.SelectModel && value == "" {
			saved = peek(key)
		}
		options := builtinqoder.Options{
			Action: action, Value: value, Previous: saved[key], ModelRef: cutMagpie,
			OwnKey:   func(token string) bool { return qoderKeyed(token, b.id) },
			Provider: func(ref string) map[string]any { return qoderProviderAt(b.id, ref, at.v1()) },
		}
		before, err := readWiringFile(path)
		if err != nil {
			return err
		}
		body, recovery, err := builtinqoder.Patch(options, before.body)
		if err != nil {
			return before.preserveOnError(path, fmt.Errorf("%s: %w", path, err))
		}
		if bytes.Equal(body, before.body) {
			err = before.check(path)
		} else {
			err = edit.WriteAtomicChecked(before.target, body, func() error { return before.check(path) })
		}
		if err != nil {
			return err
		}
		if recovery.Save {
			stash(map[string]string{key: recovery.Previous})
		}
		if recovery.Restored {
			forgetValues(saved)
		}
		return nil
	}
	onMagpie := func() bool {
		_, ok := cutMagpie(get(builtinqoder.ModelPath))
		return ok && qoderKeyed(get(slot+".apiKey"), b.id)
	}
	return &Agent{
		ID: b.id, Name: b.name, Icon: "qoder", Aliases: b.aliases, Spelled: prefixed,
		Bin: b.bin, Dir: dir, Path: path,
		// Qoder's JetBrains plugin keeps its indexer, app/ and plugins/ in
		// the same folder, and writes a settings.json of enabledPlugins
		// alone, with no CLI installed (#1495): neither is a sign of the
		// CLI. Its command, or a settings.json with any other key, is.
		// The Qoder app (Qoder-win-x64-user.exe, Qoder.app, not the IDE)
		// runs the CLI as its agent with the same folder, so magpie's
		// provider reaches it too; its settings.json can hold only
		// enabledPlugins (its bundled qoder-context plugin), and its status
		// file says it is there (#2138).
		dirShared: true,
		pathOwn:   func() bool { return !qoderPluginOnly(path) },
		sign:      func() bool { return qoderApp(dir) },
		Sync:      func() error { return apply(builtinqoder.SyncCatalog, "") },
		Notice: func() string {
			if Running(b.procs...) {
				return noticeQoder.say("agent", b.name)
			}
			return ""
		},
		Check: func() string {
			if !onMagpie() {
				return ""
			}
			return wiringOff(b.name, path, func(k string) (string, bool) { return edit.GetJSON(path, slot+"."+k) },
				"baseUrl", at.v1())
		},
		Fields: []Field{{
			Key: "model", Label: "model",
			Get: func() string { return get(builtinqoder.ModelPath) },
			Set: func(v string) error { return apply(builtinqoder.SelectModel, v) },
			Options: func(cur map[string]string) []Option {
				return withVia(ownOptions("", cur["model"]), b.id, magpieID+"/")
			},
		}, {
			// the effort of the model in use, in its model.preferences
			Key: "effort", Label: "effort",
			Get: effort,
			Set: func(v string) error { return apply(builtinqoder.SelectEffort, v) },
			Options: func(map[string]string) []Option {
				return static("low", "medium", "high", "xhigh", "max")
			},
		}},
	}
}

// qoderLevels are the efforts Qoder can ask for.
var qoderLevels = []string{"low", "medium", "high", "xhigh", "max"}

// qoderProvider is magpie's entry in Qoder's providers, every magpie model
// in it, model the one it starts on.
func qoderProvider(agent, model string) map[string]any {
	return qoderProviderAt(agent, model, gatewayV1())
}

// qoderProviderAt is qoderProvider for a Qoder reaching the gateway's /v1
// at v1.
func qoderProviderAt(agent, model, v1 string) map[string]any {
	var ms []map[string]any
	for _, m := range sharedModels(agent) {
		caps := map[string]any{"tools": true, "vision": m.Images}
		var levels []string
		for _, e := range m.Efforts {
			if slices.Contains(qoderLevels, e) && !slices.Contains(levels, e) {
				levels = append(levels, e)
			}
		}
		if len(levels) > 0 {
			caps["thinking"] = map[string]any{"modes": []string{"enabled"}, "supportsEffort": true,
				"supportedEffortLevels": levels, "requiresBudgetForEnabled": false}
		}
		e := map[string]any{"model": m.ID, "displayName": m.Name, "capabilities": caps}
		if m.Context > 0 {
			e["contextWindow"] = m.Context
		}
		if m.Output > 0 {
			e["maxOutputTokens"] = maxTokens(m)
		}
		ms = append(ms, e)
	}
	return map[string]any{"displayName": "magpie", "protocol": "openai", "baseUrl": v1,
		"apiKey": agentKeyAt(agent, v1), "model": model, "models": ms}
}

// qoderKeyed: key is one magpie gives Qoder's provider — its own,
// gateway.TokenFor, since Qoder's requests carry Bun's User-Agent and
// nothing of Qoder's, gateway.Token, which it was given before, or the LAN
// sharing key one in a WSL distro is given (agentKeyAt); a sync gives it
// the new one.
func qoderKeyed(key, agent string) bool {
	return key == gateway.TokenFor(agent) || ourKey(key)
}

// what Qoder's builds say after a change (notice.go)
var noticeQoder = newNotice("{agent} reads its settings as a session starts — open sessions keep the model they have; new ones use this.")

// qoderApp reports whether the Qoder app keeps its status in dir: it
// writes .qoder-app-status.json (-canary-, -dev- for its other channels)
// into ~/.qoder, or ~/.qoder-cn for the China build, as it signs in, and
// runs Qoder's CLI as its agent with that folder's settings.
func qoderApp(dir string) bool {
	for _, f := range []string{".qoder-app-status.json", ".qoder-app-canary-status.json", ".qoder-app-dev-status.json"} {
		if statFile(filepath.Join(dir, f)) {
			return true
		}
	}
	return false
}

// qoderPluginOnly reports whether the settings file at path is the one
// Qoder's JetBrains plugin writes, an object holding enabledPlugins and
// nothing else. A file that can't be read or parsed isn't known to be
// that, so it says false and the file still counts as the CLI's.
func qoderPluginOnly(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil || len(m) == 0 {
		return false
	}
	for k := range m {
		if k != "enabledPlugins" {
			return false
		}
	}
	return true
}
