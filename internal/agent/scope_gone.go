package agent

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/extension"
	builtinpi "github.com/yetone/magpie/internal/extension/builtin/pi"
	builtinstepcode "github.com/yetone/magpie/internal/extension/builtin/stepcode"
)

// An enabledModels list (Pi's and its forks', StepCode's, omp's) names
// models as provider/model. An entry that names one outright, when no model
// has that name, gets "No models match pattern" from Pi every time it starts
// (model-resolver.js resolveModelScope); it never takes such an entry out.
// magpie's catalog sync takes a model out of the provider block it writes
// when it is deselected in a provider's editor, or its provider goes, and
// then the list's entries on it named nothing (#2055, misaka9981).
//
// After the sync, an entry that names one of magpie's models outright (no
// glob; a thinking level on its end is read off) whose model magpie no
// longer gives the agent leaves the list. It may be the user's own, so it is
// remembered, and goes back on the list's end once magpie gives that model
// again. Globs, entries on other providers and entries on a model magpie
// still gives stay as they are, in their order. A list left with nothing is
// removed, never written empty, and comes back with its entries. A list the
// user removed themselves isn't made again.

// scopeGone is what a sync took out of an agent's enabledModels.
type scopeGone struct {
	Entries []string `json:"entries,omitempty"`
	// the sync removed the list, which had nothing else
	Emptied bool `json:"emptied,omitempty"`
}

func scopeGoneLoad(key string) scopeGone {
	var g scopeGone
	if raw := stashShared()[key]; raw != "" {
		json.Unmarshal([]byte(raw), &g)
	}
	return g
}

func scopeGoneSave(key string, g scopeGone) {
	v := ""
	if len(g.Entries) > 0 {
		b, _ := json.Marshal(g)
		v = string(b)
	}
	stash(map[string]string{key: v})
}

// scopeGoneRename moves what a sync remembered off a renamed provider, as
// RenameRefs moves the list's own entries.
func scopeGoneRename(key, from, to string) {
	g := scopeGoneLoad(key)
	if next, ok := renameRefList(g.Entries, from, to); ok {
		g.Entries = next
		scopeGoneSave(key, g)
	}
}

// scopeMagpieOne is the model an entry names outright on magpie, lowercased
// and with its level off (bare cuts the level); "" for a glob or an entry on
// another provider.
func scopeMagpieOne(p string, bare func(string) string) string {
	p = strings.ToLower(bare(strings.TrimSpace(p)))
	if strings.ContainsAny(p, "*?[") || !strings.HasPrefix(p, magpieID+"/") {
		return ""
	}
	return p
}

// piBare is a Pi entry without its thinking level.
func piBare(p string) string {
	return builtinpi.Bare(p)
}

// scopeSynced is an enabledModels list, has false when there is none, once
// it has caught up with offered, the models magpie gives the agent as
// magpie/<id> lowercased: what goes, what comes back, and what is
// remembered after.
func scopeSynced(cur []string, has bool, offered map[string]bool, mem scopeGone, bare func(string) string) (next []string, nextHas bool, out scopeGone, changed bool) {
	if !has && !mem.Emptied {
		// no list, or one the user removed: nothing to take out or put back
		return cur, has, scopeGone{}, false
	}
	in := map[string]bool{}
	for _, p := range cur {
		one := scopeMagpieOne(p, bare)
		if one != "" && !offered[one] {
			if !slices.Contains(out.Entries, p) {
				out.Entries = append(out.Entries, p)
			}
			continue
		}
		next = append(next, p)
		if one != "" {
			in[one] = true
		}
	}
	for _, p := range mem.Entries {
		one := scopeMagpieOne(p, bare)
		switch {
		case one == "", in[one] && offered[one]:
			// the list names it again already
		case offered[one]:
			next = append(next, p)
			in[one] = true
		case !slices.Contains(out.Entries, p):
			out.Entries = append(out.Entries, p)
		}
	}
	nextHas = len(next) > 0
	out.Emptied = !nextHas && len(out.Entries) > 0
	changed = nextHas != has || !slices.Equal(next, cur)
	return next, nextHas, out, changed
}

func offeredSet(n int, id func(int) string) map[string]bool {
	out := map[string]bool{}
	for i := range n {
		if v := strings.TrimSpace(id(i)); v != "" {
			out[strings.ToLower(magpieID+"/"+v)] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// piScopeSynced catches Pi's enabledModels (settings.json) up with the
// models magpie's block in models.json gives it; mem is the stash key of
// what was taken out.
func piScopeSynced(settings, modelsPath, mem string) error {
	saved := scopeGoneLoad(mem)
	var remembered *scopeGone
	update := func(state builtinpi.ScopeSettings) ([]string, bool) {
		offered := offeredSet(len(state.Offered), func(index int) string { return state.Offered[index] })
		if offered == nil {
			return nil, false
		}
		next, nextHas, out, changed := scopeSynced(state.Patterns, state.Present, offered, saved, piBare)
		if !state.Present && nextHas && state.Default != "" && !slices.ContainsFunc(next, func(pattern string) bool {
			return piTakes(pattern, state.Default)
		}) {
			next = append(next, state.Default)
		}
		remembered = &out
		return next, changed
	}
	err := extension.ApplyLinkedAgentPlan(update, builtinpi.PlanScope,
		extension.LinkedActionFile{Name: builtinpi.SettingsFile, Path: settings},
		extension.LinkedActionFile{Name: builtinpi.CatalogFile, Path: modelsPath})
	if err != nil {
		return err
	}
	if remembered != nil {
		scopeGoneSave(mem, *remembered)
	}
	return nil
}

// stepcodeScopeSynced is piScopeSynced for StepCode's enabledModels
// (config.toml). The wiring state's last list follows when the list was
// still the one magpie last wrote, so a disconnect still knows it as
// magpie's and puts back the user's own.
func stepcodeScopeSynced(at place, path, modelsPath, mem string, state *stepcodeState) error {
	saved := scopeGoneLoad(mem)
	var remembered *scopeGone
	stateChanged := false
	options := builtinstepcode.ScopeOptions{
		Catalog: func(current string) (any, bool) {
			next := theirsKeptValue(current, stepcodeProvider(at), "models")
			return next, !sameJSON(current, next)
		},
		Update: func(cfg builtinstepcode.Settings, ids []string) ([]string, bool) {
			offered := offeredSet(len(ids), func(index int) string { return ids[index] })
			if offered == nil {
				return nil, false
			}
			var cur []string
			has := cfg.Scope != nil
			if has {
				cur = *cfg.Scope
				if len(cur) == 0 {
					return nil, false
				}
			}
			next, nextHas, out, changed := scopeSynced(cur, has, offered, saved, piBare)
			if !has && nextHas {
				if def := cfg.Startup(); def != "" && !slices.ContainsFunc(next, func(pattern string) bool { return piTakes(pattern, def) }) {
					next = append(next, def)
				}
			}
			remembered = &out
			if changed && state.LastHasScope == has && slices.Equal(state.LastScope, cur) {
				state.LastHasScope, state.LastScope = nextHas, slices.Clone(next)
				stateChanged = true
			}
			return next, changed
		},
	}
	if err := extension.ApplyLinkedAgentPlan(options, builtinstepcode.PlanScope,
		extension.LinkedActionFile{Name: builtinstepcode.ConfigFile, Path: path},
		extension.LinkedActionFile{Name: builtinstepcode.CatalogFile, Path: modelsPath}); err != nil {
		return err
	}
	if stateChanged {
		if err := stepcodeStoreState(at, state); err != nil {
			return err
		}
	}
	if remembered != nil {
		scopeGoneSave(mem, *remembered)
	}
	return nil
}
