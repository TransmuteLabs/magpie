package agent

// Grok Build, xAI's grok CLI, keeps its settings in ~/.grok/config.toml (or
// $GROK_HOME's): the model new sessions start with under [models] default,
// and models of the user's own as [model."<id>"] tables. magpie adds one
// such table per catalog model, named "magpie/<provider>/<model>", pointing
// at the gateway with magpie's own key — a model with no key of its own
// would be sent the user's xAI sign-in — so the catalog joins Grok's own
// models in its /model picker.
//
// A signed-in Grok also takes remote "campaign" patches from xAI, applied
// above config.toml, and a launch campaign sets models.default (September
// 2026's grok-4.7-launch did): a default picked in magpie was ignored and
// every new session started on the campaign's model. So a default picked
// here turns campaigns off ([features] campaigns = false), and clearing it
// gives them back.

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtingrok "github.com/yetone/magpie/internal/extension/builtin/grok"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func grok(home string) *Agent { return grokIn(here(home)) }

// grokIn is Grok Build at a place: this machine's home, or a WSL distro's
// (see wsl.go), where GROK_HOME isn't read and its models name the gateway
// as the distro reaches it.
func grokIn(at place) *Agent {
	return grokLikeIn(grokBuild, at)
}

// grokKind is an agent that keeps Grok Build's config.toml: Grok Build
// itself, or a fork of it (Kigi) with a home and a name of its own.
type grokKind struct {
	id, name, icon string
	aliases, ua    []string
	// env names the home it is moved to, else dir under the user's home
	env, dir string
	// proc is the pattern of its running process (Running), and notice
	// what it says after a change while one runs
	proc   string
	notice notice
	// options are its own models and magpie's, for the model field
	options func(dir, path, cur string) []Option
}

var grokBuild = grokKind{
	id: "grok", name: "Grok Build", icon: "xai",
	aliases: []string{"grok-build", "grok-cli"},
	ua:      []string{"grok-shell", "grok-pager", "xai-grok-build"}, // grok-pager: its terminal front end
	env:     "GROK_HOME", dir: ".grok",
	proc: `(^|/)grok( |$)`, notice: noticeGrok,
	options: func(dir, path, cur string) []Option {
		// magpie's rows for the account Grok Build is signed in
		// to are its own models a second time: they fold into
		// one row (Fate on Discord: grokbuild 在登录态下会加载重复的模型)
		return joinedVia(grokOwnOptions(dir, path, cur), grokVia(), [2]string{"grok same", dir + "\x00" + path})
	},
}

// grokLikeIn is the agent of kind k at a place.
func grokLikeIn(k grokKind, at place) *Agent {
	dir := at.getenv(k.env)
	if dir == "" {
		dir = at.join(k.dir)
	}
	path := filepath.Join(dir, builtingrok.SettingsFile)
	get := func(key string) (string, error) {
		models, err := edit.GetTOMLTable(path, "models")
		return models[key], err
	}
	keyWindows, keyPercents := k.id+":"+path+":windows", k.id+":"+path+":percents"
	apply := func(action builtingrok.Action, value string) error {
		options := builtingrok.Options{
			Action: action, Name: k.name, Value: value,
		}
		if action == builtingrok.SelectModel {
			if ref, ok := cutMagpie(value); ok {
				options.GatewayModel = ref
			}
		}
		if options.GatewayModel != "" || action == builtingrok.SyncCatalog {
			saved := stashShared()
			options.Windows, options.Percents = saved[keyWindows], saved[keyPercents]
			options.Catalog = func() builtingrok.Catalog {
				url := at.v1()
				return builtingrok.Catalog{
					URL: url, Key: keyAt(url), Models: sharedModels(k.id),
					Working: settings.Load().ForAgent(k.id).Working,
				}
			}
		}
		var recovery builtingrok.Recovery
		err := extension.ApplyLinkedAgentPlan(options, func(options builtingrok.Options, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
			plan, pending, err := builtingrok.Plan(options, settings)
			recovery = pending
			return plan, err
		}, extension.LinkedActionFile{Name: builtingrok.SettingsFile, Path: path})
		if err != nil {
			return err
		}
		if recovery.Forget {
			forget(keyWindows, keyPercents)
		} else if recovery.Windows != "" || recovery.Percents != "" {
			put := map[string]string{}
			if recovery.Windows != "" {
				put[keyWindows] = recovery.Windows
			}
			if recovery.Percents != "" {
				put[keyPercents] = recovery.Percents
			}
			stash(put)
		}
		return nil
	}
	// the efforts of a model through magpie, as its catalog entry has them
	efforts := func(model string) []string {
		ref, ok := strings.CutPrefix(model, magpieID+"/")
		if !ok {
			return nil
		}
		return catalog.Efforts(sharedModels(k.id), ref) // only read
	}
	agent := &Agent{
		ID: k.id, Name: k.name, Icon: k.icon, Aliases: k.aliases, Spelled: prefixed,
		UA:  k.ua,
		Dir: dir, Path: path,
		Sync: func() error { return apply(builtingrok.SyncCatalog, "") },
		Notice: func() string {
			if Running(k.proc) {
				return k.notice.String()
			}
			return ""
		},
		Check: func() string {
			v, err := get("default")
			if err != nil {
				return err.Error()
			}
			if !usesMagpie(v) {
				return ""
			}
			t, err := edit.GetTOMLTable(path, "model."+strconv.Quote(v))
			if err != nil {
				return err.Error()
			}
			if t == nil {
				return k.name + "'s [model." + strconv.Quote(v) + "] (config.toml) is gone, so it no longer reaches magpie"
			}
			return wiringOff(k.name, path, func(k string) (string, bool) { v, ok := t[k]; return v, ok },
				"base_url", at.v1(), "api_key", at.gwKey())
		},
		Fields: []Field{
			{
				Key: "model", Label: "model",
				Get: func() string { v, _ := get("default"); return v },
				Set: func(v string) error { return apply(builtingrok.SelectModel, v) },
				Options: func(cur map[string]string) []Option {
					return k.options(dir, path, cur["model"])
				},
			},
			{
				// the effort new sessions start with; Grok applies it to a
				// model that supports it and ignores it otherwise
				Key: "effort", Label: "effort",
				Get: func() string { v, _ := get("default_reasoning_effort"); return v },
				Set: func(v string) error { return apply(builtingrok.SetEffort, v) },
				Options: func(cur map[string]string) []Option {
					if es := efforts(cur["model"]); es != nil {
						return static(es...)
					}
					return static("low", "medium", "high")
				},
			},
		},
	}
	extensionFenceLocalAtomic(agent, []string{path})
	return agent
}

// grokOwnOptions are the models Grok Build offers its signed-in account: as
// it last listed them itself (models_cache.json in its home), and as magpie
// last listed them for a Grok subscription; then the models of the user's
// own config.toml tables, and the current one. Grok asks for them itself,
// on its sign-in or their own key, so each says so (Direct): a row not
// connected listed nothing of Grok's own, only magpie's, unless a Grok
// subscription was signed in in magpie (EZN7L2C3, #834).
// grokVia is magpie's rows for Grok Build, each marked Same where it is
// the account Grok Build is signed in to, kept as viaMagpieShared keeps
// its list: a copy marked again for every look was most of what the
// window's state spent on Grok Build. Read only.
func grokVia() []Option {
	return builtOptions.shared("grok\x00"+magpieID+"/\x00same", func() []Option {
		opts := buildVia("grok", magpieID+"/")
		for i := range opts {
			opts[i].Same = opts[i].own
		}
		return opts
	})
}

// grokCached is a model in Grok Build's models_cache.json that it shows.
type grokCached struct{ id, name string }

// grokCache is models_cache.json's shown models, read again only once the
// file changed: it is a few hundred KB, read and walked for every look.
func grokCache(path string) []grokCached {
	ms, _ := filememo.Read("grok models cache", path, func(raw []byte) ([]grokCached, error) {
		var out []grokCached
		gjson.GetBytes(raw, "models").ForEach(func(k, v gjson.Result) bool {
			if !v.Get("info.hidden").Bool() {
				out = append(out, grokCached{k.String(), v.Get("info.name").String()})
			}
			return true
		})
		return out, nil
	})
	return ms
}

func grokOwnOptions(dir, path, cur string) []Option {
	seen := map[string]bool{}
	var out []Option
	add := func(id, name, direct string) {
		if id == "" || seen[id] || strings.HasPrefix(id, magpieID+"/") {
			return
		}
		seen[id] = true
		o := Option{Value: id, Icon: "xai", Direct: direct}
		if name != "" && name != id {
			o.Label = name
		}
		out = append(out, o)
	}
	// dir and path empty: a stopped distro's, whose files aren't read (#3896)
	if dir != "" {
		for _, m := range grokCache(filepath.Join(dir, "models_cache.json")) {
			add(m.id, m.name, "xAI")
		}
	}
	ms, _, _ := catalog.Live("grok")
	for _, m := range ms {
		add(m.ID, "", "xAI")
	}
	// the user's own [model."<id>"] tables, each on its own endpoint
	if path != "" {
		_ = edit.EachTOMLTable(path, func(t edit.TOMLTable) {
			id, ok := strings.CutPrefix(t.Name(), "model.")
			if !ok {
				return
			}
			if u, err := strconv.Unquote(id); err == nil {
				id = u
			}
			direct := "xAI"
			if h := hostOf(t.Scalar("base_url")); h != "" {
				direct = h
			}
			add(id, t.Scalar("name"), direct)
		})
	}
	if cur != "" && !seen[cur] && !strings.HasPrefix(cur, magpieID+"/") {
		out = append([]Option{{Value: cur, Icon: modelIcon("", cur), Direct: "xAI"}}, out...)
	}
	return group("Grok Build", out)
}

// what grok says after a change (notice.go)
var (
	noticeGrok = newNotice("Grok Build reads its settings at start-up — restart open grok sessions to use this.")
)
