package agent

// Command Code (commandcode.ai) keeps its settings in ~/.commandcode/
// settings.json, the model as "provider/model", and takes providers of the
// user's own in ~/.commandcode/providers.json. magpie adds itself there as
// the provider "magpie", keyless, with the catalog as its models; a model
// through magpie is "magpie/<provider>/<model>", which Command Code splits
// at the first "/". Command Code still wants its own sign-in for these.

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtincommandcode "github.com/yetone/magpie/internal/extension/builtin/commandcode"
)

// ccEfforts are the reasoning levels Command Code knows.
var ccEfforts = []string{"low", "medium", "high", "xhigh", "max"}

func commandCode(home string) *Agent { return commandCodeIn(here(home)) }

// commandCodeIn is Command Code at a place: this machine's home, or a WSL
// distro's (see wsl.go), its provider naming the gateway as the distro
// reaches it.
func commandCodeIn(at place) *Agent {
	dir := at.join(".commandcode")
	path := filepath.Join(dir, "settings.json")
	providers := filepath.Join(dir, "providers.json")
	get := func(k string) string { v, _ := edit.GetJSON(path, k); return v }
	configFile := extension.LinkedActionFile{Name: builtincommandcode.SettingsFile, Path: path}
	catalogFile := extension.LinkedActionFile{Name: builtincommandcode.CatalogFile, Path: providers}
	catalog := builtincommandcode.CatalogOptions{Value: func(current string) (any, bool) {
		value := theirsKeptValue(current, ccProviderJSONAt(at.v1()), "models")
		return value, !sameJSON(current, value)
	}}
	return &Agent{
		ID: "commandcode", Name: "Command Code", Icon: "commandcode", Aliases: []string{"command-code", "cmd"}, Spelled: prefixed,
		UA:  []string{"command-code", "commandcode"},
		Bin: "command-code", Dir: dir, Path: path,
		ListOnly: true,
		Sync: func() error {
			return extension.ApplyLinkedAgentPlan(catalog, builtincommandcode.PlanSync, catalogFile)
		},
		Notice: func() string {
			notes := []string{noticeCommandcode1.String()}
			if Running(`(^|/)(cmd|cmdc|command-code|commandcode)( |$)`) {
				notes = append(notes, noticeCommandcode2.String())
			}
			return strings.Join(notes, " ")
		},
		Check: func() string {
			if !usesMagpie(get("model")) {
				return ""
			}
			if p := get("modelProvider"); p != magpieID {
				return "Command Code's modelProvider (settings.json) is " + orDefault(p) + ", so it no longer asks magpie"
			}
			return wiringOff("Command Code", providers, func(k string) (string, bool) { return edit.GetJSON(providers, "provider."+magpieID+"."+k) },
				"baseURL", at.v1())
		},
		Fields: []Field{{
			Key: "model", Label: "model",
			Get: func() string { return get("model") },
			Set: func(v string) error {
				options := builtincommandcode.ModelOptions{Model: v}
				if ref, ok := strings.CutPrefix(v, magpieID+"/"); ok && isMagpie(ref) {
					options.Catalog = catalog
				}
				return extension.ApplyLinkedAgentPlan(options, builtincommandcode.PlanModel, configFile, catalogFile)
			},
			Options: func(cur map[string]string) []Option {
				var out []Option
				if v := cur["model"]; v != "" && !usesMagpie(v) {
					out = append(out, Option{Value: v, Icon: modelIcon("", v)})
				}
				return withVia(out, "commandcode", magpieID+"/")
			},
		}, {
			// settings.json's reasoningEffort keeps an effort for each model
			// (Command Code's /effort saves it); this is the current model's
			Key: "effort", Label: "effort",
			Get: func() string { return ccEffortMap(path)[get("model")] },
			Set: func(v string) error {
				return extension.ApplyLinkedAgentPlan(v, builtincommandcode.PlanEffort, configFile)
			},
			Options: func(cur map[string]string) []Option {
				if ref, ok := strings.CutPrefix(cur["model"], magpieID+"/"); ok {
					for _, m := range sharedModels("commandcode") {
						if m.ID != ref {
							continue
						}
						var efforts []string
						for _, x := range ccEfforts {
							if slices.Contains(m.Efforts, x) {
								efforts = append(efforts, x)
							}
						}
						if len(efforts) > 0 {
							return static(efforts...)
						}
					}
				}
				return static(ccEfforts...)
			},
		}},
	}
}

// ccProviderJSONAt is magpie's entry in providers.json, for a Command Code
// reaching the gateway's /v1 at v1. The key is false: the gateway takes
// any, and Command Code refuses one written out.
func ccProviderJSONAt(v1 string) any {
	ms := map[string]any{}
	for _, m := range sharedModels("commandcode") {
		e := map[string]any{"name": m.Name}
		var efforts []string
		for _, x := range m.Efforts {
			if slices.Contains(ccEfforts, x) {
				efforts = append(efforts, x)
			}
		}
		// a model that thinks with no levels magpie knows (Trae CN's
		// kimi-k3-max, #2380) is marked reasoning alone, which Command
		// Code takes for low, medium and high (its BYOK reference); the
		// gateway fits them to the model
		switch {
		case len(efforts) > 0:
			e["reasoning"] = true
			e["reasoningEfforts"] = efforts
		case len(m.Efforts) == 0 && m.Reasoning:
			e["reasoning"] = true
		}
		// Command Code reads the window and the reply limit from this file
		// alone — its own catalog keys on the bare vendor id and never
		// matches "magpie/provider/model" (#5567) — and without them serves
		// every model as its 200K default, compacting a 1M-window one long
		// before it has to. Its names for the pair are contextWindow and
		// maxOutput (its BYOK reference).
		if m.Context > 0 {
			e["contextWindow"] = m.Context
		}
		if m.Output > 0 {
			e["maxOutput"] = maxTokens(m)
		}
		ms[m.ID] = e
	}
	return map[string]any{"name": "magpie", "api": "openai-completions", "baseURL": v1, "apiKey": false, "models": ms}
}

// ccEffortMap is settings.json's reasoningEffort, model id to effort.
func ccEffortMap(path string) map[string]string {
	out := map[string]string{}
	raw, _ := edit.GetJSON(path, "reasoningEffort")
	var values map[string]any
	if json.Unmarshal([]byte(raw), &values) != nil {
		return out
	}
	for k, v := range values {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// what commandcode says after a change (notice.go)
var (
	noticeCommandcode1 = newNotice("Command Code wants its own sign-in (cmd login) even for models through magpie.")
	noticeCommandcode2 = newNotice("It reads its settings at start-up — restart open Command Code sessions to use this.")
)
