package agent

import (
	"encoding/json"
	"strings"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtinomp "github.com/yetone/magpie/internal/extension/builtin/omp"
	sdk "github.com/yetone/magpie/pkg/extension"
	"gopkg.in/yaml.v3"
)

// omp's enabledModels (config.yml) is, as Pi's is (pi.go), the list a new
// session is held to while it names anything: the default role starts only
// when the list takes its model in, else omp picks another model from the
// list (main.ts buildSessionOptions, sdk.ts resolveAllowedModels and
// pickDefaultAvailableModel), so a model magpie picks outside it never
// starts (#1769). omp's other roles are resolved against every model it has
// (task/executor.ts resolveModelOverride, session/model-controls.ts
// resolveRoleModelFull, both on modelRegistry.getAvailable()), so only the
// default role is held to the list.
//
// The list is the user's: magpie adds the default role's model to it as Pi's
// fix did (#160), keeps the entries, their order and comments,
// and never makes a list where there is none or an empty one. Its plain
// entries hold in every folder; an entry scoped to folders ({path, models})
// is the user's list for those projects and left as it is.

// ompEvery is the entry magpie adds to enabledModels for its own models:
// every model magpie writes into models.yml for omp, which are the ones its
// Agents page offers. omp matches a glob with Bun.Glob, whose * stops at a
// slash, and a model of magpie's is magpie/<provider>/<model>, so it takes
// **. With the default role's model alone there, omp's picker and Ctrl+P
// left out every other model magpie gives it (#1769, real-jacket).
const ompEvery = builtinomp.AllGatewayModels

// ompTakes reports whether an enabledModels entry surely takes in model, a
// provider/model with no level: Pi's rules (piTakes), with the levels omp
// reads off an entry's end, auto among them, and a trailing ** taking in
// everything under it, slashes and all, as Bun.Glob's does.
func ompTakes(pattern, model string) bool {
	p, _ := ompLevel(strings.TrimSpace(pattern))
	if base, ok := strings.CutSuffix(p, "**"); ok && !strings.ContainsAny(base, "*?[") && (base == "" || strings.HasSuffix(base, "/")) {
		return strings.HasPrefix(strings.ToLower(model), strings.ToLower(base))
	}
	return piTakes(p, model)
}

// ompScopeTakes says whether the plain entries take in model; true when
// there are none, as then nothing holds a session to them.
func ompScopeTakes(plain []string, model string) bool {
	if len(plain) == 0 {
		return true
	}
	for _, p := range plain {
		if ompTakes(p, model) {
			return true
		}
	}
	return false
}

// ompScopeOwnMagpie says whether the user chose for themselves which of
// magpie's models the list holds: an entry on magpie with a glob or a
// thinking level, or a glob of theirs that takes model in (deepseek/*,
// which omp matches against the id alone), is theirs, not one magpie added.
func ompScopeOwnMagpie(plain []string, model string) bool {
	for _, p := range plain {
		m, level := ompLevel(strings.TrimSpace(p))
		glob := strings.ContainsAny(m, "*?[")
		if glob && ompTakes(m, model) || strings.HasPrefix(strings.ToLower(m), magpieID+"/") && (glob || level != "") {
			return true
		}
	}
	return false
}

// ompScopeModel lets a session start on model, a provider/model, when
// config.yml's enabledModels has plain entries and none takes it in. For
// one of magpie's it is ompEvery that joins the end, in place of the
// models of magpie's the list names one by one (what magpie added before),
// so omp's picker has every model magpie offers it beside the user's own;
// unless the user picked which of magpie's the list holds
// (ompScopeOwnMagpie), when only model joins it. Another model joins as
// itself.
func ompScopeModel(model string) func(*yaml.Node) (bool, bool) {
	return func(n *yaml.Node) (bool, bool) {
		plain := builtinomp.PlainScope(n)
		if len(plain) == 0 {
			return false, false
		}
		onMagpie := strings.HasPrefix(strings.ToLower(model), magpieID+"/")
		if !onMagpie || ompScopeOwnMagpie(plain, model) {
			if ompScopeTakes(plain, model) {
				return false, false
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: model})
			return true, false
		}
		kept := n.Content[:0:0]
		for _, c := range n.Content {
			if c.Kind == yaml.ScalarNode && ompScopeMagpieOne(c.Value) {
				continue
			}
			kept = append(kept, c)
		}
		n.Content = append(kept, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ompEvery})
		return true, false
	}
}

// ompScopeMagpieOne is a plain entry naming one of magpie's models, with
// no glob or level: what magpie adds, or ompEvery takes in all the same.
func ompScopeMagpieOne(v string) bool {
	v = strings.TrimSpace(v)
	return !strings.ContainsAny(v, "*?[") && strings.HasPrefix(strings.ToLower(v), magpieID+"/")
}

func ompRenameRefs(path, gone string, roles []string, from, to string) (bool, error) {
	originals := peek(roles...)
	priorScope := peek(gone)[gone]
	saved := ompReadScope(priorScope)
	options := builtinomp.RenameOptions{From: from, To: to, Originals: originals}
	var moved bool
	var updates map[string]string
	err := extension.ApplyLinkedAgentPlan(options, func(options builtinomp.RenameOptions, snapshot sdk.ActionSettings) (sdk.AgentPlan, error) {
		plan, changed, next, err := builtinomp.PlanRename(options, snapshot)
		moved, updates = changed, next
		return plan, err
	}, extension.LinkedActionFile{Name: builtinomp.ConfigFile, Path: path})
	if err != nil {
		return false, err
	}
	if entries, renamed := renameRefList(saved.Entries, from, to); renamed {
		saved.Entries = entries
		updates[gone] = ompScopeText(saved)
	}
	originals[gone] = priorScope
	ompUpdateStash(originals, updates)
	return moved, nil
}

func ompBare(value string) string {
	model, _ := builtinomp.Level(value)
	return model
}

func ompReadScope(raw string) scopeGone {
	var state scopeGone
	if raw != "" {
		json.Unmarshal([]byte(raw), &state)
	}
	return state
}

func ompScopeText(state scopeGone) string {
	if len(state.Entries) == 0 {
		return ""
	}
	body, _ := json.Marshal(state)
	return string(body)
}

func ompUpdateStash(before, updates map[string]string) {
	if len(updates) == 0 {
		return
	}
	stashMu.Lock()
	defer stashMu.Unlock()
	saved := stashLoad()
	changed := false
	for key, value := range updates {
		if saved[key] != before[key] || saved[key] == value {
			continue
		}
		if value == "" {
			delete(saved, key)
		} else {
			saved[key] = value
		}
		changed = true
	}
	if changed {
		stashWrite(saved)
	}
}

// ompScopeLeavesOut is the default role's model when enabledModels has
// plain entries and none takes it in: a new session would start on another.
func ompScopeLeavesOut(cfg, role string) string {
	model, _, one := builtinomp.SplitRole(role)
	if !one || model == "" || ompScopeTakes(edit.GetYAMLList(cfg, "enabledModels"), model) {
		return ""
	}
	return model
}
