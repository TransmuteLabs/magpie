package agent

// Qwen Code (`qwen`, QwenLM/qwen-code, a Gemini CLI fork) keeps its
// settings in ~/.qwen/settings.json ($QWEN_HOME when set). Models of the
// user's own are the modelProviders.<protocol> arrays, each
// {baseUrl, envKey, id, name}: id is the model asked for, name what its
// /model picker shows, and envKey names a variable in the same file's env
// object (or ~/.qwen/.env) with the key. The model sessions start on is
// settings.model {name, baseUrl}, baseUrl telling two entries of one id
// apart, and security.auth.selectedType "openai" skips the /auth prompt
// on start.
//
// magpie appends one entry per catalog model to modelProviders.openai,
// the wire id the gateway takes ("<provider>/<model>") and the name
// "magpie/<provider>/<model>", every one on the same env key of its own,
// which is how an entry of magpie's is told from the user's own, and
// points settings.model and security.auth.selectedType at a pick. The key
// itself lives in the settings' env object — a shell-exported or
// ~/.qwen/.env variable of the same name would win over it, which the
// qwen-specific name makes unlikely. The user's entries stay as they were
// and in front, so their own ids don't move, and the default, auth type
// and any key of theirs they had are stashed and put back when magpie
// steps out.

import (
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/jsonc"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtinqwen "github.com/yetone/magpie/internal/extension/builtin/qwen"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// qwenEntriesAt are magpie's entries for a qwen reaching the gateway at
// v1.
func qwenEntriesAt(v1 string) []builtinqwen.Entry {
	var out []builtinqwen.Entry
	for _, m := range sharedModels("qwen") {
		// name is what /model shows, the id beside it when they differ:
		// the model's name as every agent's list calls it, by the Provider
		// in model names setting (magpie/<id> before, which none of the
		// setting's ways changed)
		e := builtinqwen.Entry{ID: m.ID, Name: m.Name, BaseURL: v1, EnvKey: builtinqwen.EnvKey}
		if m.Context > 0 {
			e.Generation = &builtinqwen.Generation{ContextWindowSize: m.Context}
		}
		out = append(out, e)
	}
	return out
}

// qwenCustoms reads modelProviders.openai of a settings.json.
func qwenCustoms(path string) []builtinqwen.Custom {
	raw, err := edit.Read(path)
	if err != nil || len(raw) == 0 {
		return nil
	}
	return builtinqwen.Customs(jsonc.ToJSONInPlace(raw))
}

// qwenModelOf reads settings.model: the model's id, and the baseUrl
// telling one of its two entries apart ("" when unset).
func qwenModelOf(path string) (name, base string) {
	name, _ = edit.GetJSON(path, builtinqwen.Model)
	base, _ = edit.GetJSON(path, builtinqwen.ModelBase)
	return name, base
}

func qwen(home string) *Agent { return qwenIn(here(home)) }

// qwenIn is Qwen Code at a place: this machine's home, or a WSL distro's
// (see wsl.go), whose entries name the gateway as the distro reaches it,
// with the key it takes from there.
func qwenIn(at place) *Agent {
	dir := at.join(".qwen")
	if d := at.getenv("QWEN_HOME"); d != "" {
		dir = d
	}
	path := filepath.Join(dir, "settings.json")
	stashPrefix := "qwen:" + path + ":"
	apply := func(options builtinqwen.Options) error {
		options.Key, options.Gateway = at.gwKey(), at.v1()
		options.Catalog = func() []builtinqwen.Entry { return qwenEntriesAt(options.Gateway) }
		options.Previous = map[string]string{}
		saved := stashShared()
		for _, key := range []string{builtinqwen.Model, builtinqwen.ModelBase, builtinqwen.Env, builtinqwen.Auth, builtinqwen.AuthSeen} {
			options.Previous[key] = saved[stashPrefix+key]
		}
		var recovery builtinqwen.Recovery
		err := extension.ApplyLinkedAgentPlan(options, func(options builtinqwen.Options, snapshot sdk.ActionSettings) (sdk.AgentPlan, error) {
			plan, changes, err := builtinqwen.Plan(options, snapshot)
			recovery = changes
			return plan, err
		}, extension.LinkedActionFile{Name: builtinqwen.SettingsFile, Path: path})
		if err != nil {
			return err
		}
		if len(recovery.Save) > 0 {
			values := make(map[string]string, len(recovery.Save))
			for key, value := range recovery.Save {
				values[stashPrefix+key] = value
			}
			stash(values)
		}
		if len(recovery.Forget) > 0 {
			values := make(map[string]string, len(recovery.Forget))
			for _, key := range recovery.Forget {
				if value := options.Previous[key]; value != "" {
					values[stashPrefix+key] = value
				}
			}
			forgetValues(values)
		}
		return nil
	}

	// get spells an entry of magpie's as the catalog does
	get := func() string {
		name, base := qwenModelOf(path)
		if name == "" {
			return ""
		}
		if entry := builtinqwen.Find(qwenCustoms(path), name, base); entry != nil && entry.Owned {
			return magpieID + "/" + entry.ID
		}
		return name
	}

	agent := &Agent{
		ID: "qwen", Name: "Qwen Code", Icon: "qwen-color", Aliases: []string{"qwen-code", "qwen-cli"}, Spelled: prefixed,
		// qwen's requests carry QwenCode/<version> (packages/core/src/core/
		// openaiContentGenerator/provider/default.ts buildHeaders)
		UA:  []string{"qwencode"},
		Bin: "qwen", Dir: dir, Path: path,
		ListOnly:  true,
		Telemetry: usageStatsTelemetry(at, path, "qwen"),
		Sync: func() error {
			return apply(builtinqwen.Options{Sync: true})
		},
		Notice: func() string {
			if Running(`(^|/)qwen( |$)`) {
				return noticeQwen.String()
			}
			return ""
		},
		Check: func() string {
			// settings.model is magpie's by the entry it names, wherever
			// that entry points: another address is drift to report, not
			// a model of the user's
			name, _ := qwenModelOf(path)
			if name == "" {
				return ""
			}
			var c *builtinqwen.Custom
			cs := qwenCustoms(path)
			for i := range cs {
				if cs[i].Owned && cs[i].ID == name {
					c = &cs[i]
					break
				}
			}
			if c == nil {
				return ""
			}
			if v, _ := edit.GetJSON(path, builtinqwen.Env); !keyHolds(v, at.gwKey()) {
				return "Qwen Code's " + builtinqwen.Env + " (settings.json) no longer is magpie's key, so magpie's models have none"
			}
			if v, ok := edit.GetJSON(path, builtinqwen.Auth); !ok || v != "openai" {
				return "Qwen Code's security.auth.selectedType (settings.json) no longer is openai, so it asks /auth instead of using magpie"
			}
			return wiringOff("Qwen Code", path, func(k string) (string, bool) {
				g := gjson.GetBytes(c.Raw, k)
				return g.String(), g.Exists()
			}, "baseUrl", at.v1())
		},
		Fields: []Field{{
			Key: "model", Label: "model",
			Get: get,
			Set: func(v string) error {
				if ref, ok := strings.CutPrefix(v, magpieID+"/"); ok && isMagpie(ref) {
					return apply(builtinqwen.Options{Choice: ref, Through: true})
				}
				return apply(builtinqwen.Options{Choice: v})
			},
			Options: func(cur map[string]string) []Option {
				return withVia(qwenOwnOptions(path, cur["model"]), "qwen", magpieID+"/")
			},
		}},
	}
	extensionFenceLocalAtomic(agent, []string{path})
	return agent
}

// qwenOwnOptions are the models of the user's own the settings file has:
// every modelProviders.openai entry that isn't magpie's, and the current
// pick.
func qwenOwnOptions(path, cur string) []Option {
	seen := map[string]bool{}
	var out []Option
	for _, c := range qwenCustoms(path) {
		if c.Owned || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		label := c.Name
		if label == "" || label == c.ID {
			label = ""
		}
		out = append(out, Option{Value: c.ID, Label: label, Icon: modelIcon("", c.ID)})
	}
	if cur != "" && !seen[cur] && !strings.HasPrefix(cur, magpieID+"/") {
		out = append([]Option{{Value: cur, Icon: modelIcon("", cur)}}, out...)
	}
	return group("Qwen Code", out)
}

// what qwen says after a change (notice.go)
var (
	noticeQwen = newNotice("Qwen Code reads a session's model at start-up — start a new session, or /model anew, to use this.")
)
