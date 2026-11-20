package agent

import (
	"encoding/json"
	"errors"
	"maps"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtinopencode "github.com/yetone/magpie/internal/extension/builtin/opencode"
)

// OpenCode 2 reads OpenCode 1's config and turns it into its own words as
// it loads it (packages/core/src/config/normalize.ts at v2.2.6): provider
// into providers, small_model into agents.title.model. A model's reasoning,
// which OpenCode 2 doesn't read, it drops with a WARN on every load, one per
// model ("omitted unsupported legacy setting", kaising00 on X). Where the
// OpenCode the config is for is 2.x, magpie writes the file in OpenCode 2's
// words: magpie's provider under providers only (openCodeNative), its small
// model under agents.title.model. For OpenCode 1, or a version not known,
// it writes OpenCode 1's, which every OpenCode reads.

// openCodeVersion is the version of the opencode on PATH, "" when not
// known; a var so tests can fake it.
var openCodeVersion = func() string { return (&Agent{ID: "opencode", Bin: "opencode"}).InstalledVersion() }

// openCodeTwo is whether the OpenCode at a place is 2.x: the one on this
// machine's PATH, or the distro's, as its probe found it.
func openCodeTwo(at place) bool {
	n, ok := openCodeVersionParts(openCodeVersionAt(at))
	return ok && n[0] >= 2
}

// openCodeTitleKey is where OpenCode 2 keeps the model it titles sessions
// with, which OpenCode 1's small_model became.
const openCodeTitleKey = builtinopencode.TitleModelKey

// ocFile is an OpenCode config magpie writes its provider into: two when
// the OpenCode reading it is 2.x, block magpie's provider in OpenCode 1's
// words (magpieProviderJSONAt).
type ocFile struct {
	path     string
	two      bool
	discover bool // the OpenCode loads magpie's plugin (opencode_discover.go)
	block    func() any
}

// sync brings magpie's provider in the file up to date, and writes it
// where it is missing when put (a model of magpie's is chosen there). For
// OpenCode 2 a provider of magpie's under provider, and a small_model of
// magpie's, move to OpenCode 2's words first.
//
// Where the OpenCode finds magpie's models itself (discover), the file has
// magpie's provider without them, and magpie's plugin beside it.
func (f ocFile) sync(put bool) error {
	if f.two {
		return f.applyV2(put, nil)
	}
	var err error
	if f.discover {
		err = f.syncFound(put)
	} else {
		err = f.syncListed(put)
	}
	if !f.discover && unstashPeek(openCodeDiscoverKey(f.path)) != "" {
		stash(map[string]string{openCodeDiscoverKey(f.path): ""})
	}
	return errors.Join(err, f.pluginSynced())
}

// syncFound is sync for an OpenCode that finds magpie's models itself:
// magpie's models in the file go first, leaving what the user said of
// them (modelsLeft), and the provider is written without them.
func (f ocFile) syncFound(put bool) error {
	moved := unstashPeek(openCodeDiscoverKey(f.path)) != ""
	listed := f
	listed.block = withoutModels(f.block)
	get := func(key string) (string, bool) { return edit.GetJSON(f.path, key) }
	err := f.modelsLeft("provider."+magpieID, openCodeKept(f.path, f.block)(), f.block(), moved)
	if _, ok := get(openCodeNativeKey); ok && err == nil {
		err = f.modelsLeft(openCodeNativeKey, openCodeNativeKept(get, f.block()), openCodeNative(f.block()), moved)
	}
	if err != nil {
		return err
	}
	if !moved {
		stash(map[string]string{openCodeDiscoverKey(f.path): "1"})
	}
	return listed.syncListed(put)
}

// syncListed is sync with magpie's models as block has them.
func (f ocFile) syncListed(put bool) error {
	return syncOpenCodeV1(f.path, f.block, put, true)
}

func syncOpenCodeV1(path string, block func() any, put, mirror bool) error {
	const legacy = "provider." + magpieID
	historyKey := outputsWantedKey(path)
	history := unstashPeek(historyKey)
	nextHistory := history
	err := applyJSONSync(path, func(config gjson.Result) []edit.KV {
		get := openCodeSnapshotValues(config)
		var changes []edit.KV
		for _, key := range []string{legacy, openCodeNativeKey} {
			if key == openCodeNativeKey && !mirror {
				continue
			}
			current := config.Get(key)
			if !current.Exists() && !(key == legacy && put) {
				continue
			}
			var next any
			if key == legacy {
				next = openCodeKeptValue(block(), get, func(models orderedJSON) {
					nextHistory = planOpenCodeOutputLimits(models, get, history)
				})
			} else {
				next = openCodeNativeKept(get, block())
			}
			if !current.Exists() || !sameOrder(current.Raw, next) {
				changes = append(changes, edit.KV{Path: key, Value: next})
			}
		}
		return changes
	})
	if err == nil && nextHistory != history {
		stash(map[string]string{historyKey: nextHistory})
	}
	return err
}

func (f ocFile) applyV2(put bool, primary *builtinopencode.PrimaryModelOptions) error {
	saved := stashShared()
	discoveryKey := openCodeDiscoverKey(f.path)
	moved := saved[discoveryKey] != ""
	outputKey := outputsWantedKey(f.path)
	outputHistory := saved[outputKey]
	keepOutputs := func(block any, get func(string) (string, bool)) any {
		if provider, ok := block.(map[string]any); ok {
			if models, ok := provider["models"].(orderedJSON); ok {
				outputHistory = planOpenCodeOutputLimits(models, get, outputHistory)
			}
		}
		return block
	}
	options := builtinopencode.V2ConfigOptions{
		ConvertProvider: func(raw []byte) (json.Marshaler, bool) { return openCodeFromV1(raw) },
		PutProvider:     put,
		Primary:         primary,
	}
	block := f.block
	if f.discover {
		block = withoutModels(block)
		options.DiscoverModels = func(config gjson.Result, raw string) (json.Marshaler, bool) {
			get := openCodeSnapshotValues(config)
			kept := openCodeNativeKept(get, keepOutputs(f.block(), get))
			left, changed := openCodeModelsLeft(raw, kept, openCodeNative(f.block()), moved, true)
			if len(left) == 0 {
				return nil, changed
			}
			return left, changed
		}
	}
	var synced bool
	options.RefreshProvider = func(config gjson.Result, put bool) any {
		synced = true
		get := openCodeSnapshotValues(config)
		raw, has := get(openCodeNativeKey)
		if !has && !put {
			return nil
		}
		native := openCodeNativeKept(get, keepOutputs(block(), get))
		if has && sameJSON(raw, native) && sameOrder(raw, native) {
			return nil
		}
		return native
	}
	configFile := f.configSnapshot()
	var pluginReady bool
	if primary != nil && primary.Routed {
		configFile.BeforeWrite = func(string) error {
			if !synced {
				return nil
			}
			err := f.syncPlugin(true)
			pluginReady = err == nil
			return err
		}
	}
	if err := extension.ApplyLinkedAgentPlan(options, builtinopencode.PlanV2Config, configFile); err != nil {
		return err
	}
	if !synced {
		return nil
	}
	if outputHistory != saved[outputKey] || moved != f.discover {
		metadata := map[string]string{}
		if outputHistory != saved[outputKey] {
			metadata[outputKey] = outputHistory
		}
		if moved != f.discover {
			metadata[discoveryKey] = ""
			if f.discover {
				metadata[discoveryKey] = "1"
			}
		}
		stash(metadata)
	}
	if pluginReady {
		return nil
	}
	return f.pluginSynced()
}

func openCodeSnapshotValues(config gjson.Result) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value := config.Get(key)
		return value.String(), value.Exists()
	}
}

func (f ocFile) configSnapshot() extension.LinkedActionFile {
	return extension.LinkedActionFile{Name: builtinopencode.ConfigFile, Path: f.path}
}

// small is the file's small model as the OpenCode reading it takes it:
// for OpenCode 2 its title agent's, else small_model.
func (f ocFile) small() string {
	if f.two {
		if v, ok := edit.GetJSONRaw(f.path, openCodeTitleKey); ok {
			return openCodeSelection(v)
		}
	}
	v, _ := edit.GetJSON(f.path, "small_model")
	return v
}

// setSmall sets the file's small model: for OpenCode 2 its title agent's,
// with a small_model, which it would read only without one, gone.
func (f ocFile) setSmall(v string) error {
	options := builtinopencode.SmallModelOptions{Two: f.two, Model: v}
	return extension.ApplyLinkedAgentPlan(options, builtinopencode.PlanSmallModel, f.configSnapshot())
}

func (f ocFile) setPrimary(model, gateway string, checkChamber bool) error {
	ref, prefixed := strings.CutPrefix(model, magpieID+"/")
	var dropProvider bool
	pick := builtinopencode.PrimaryModelOptions{
		Model:  model,
		Routed: prefixed && isMagpie(ref),
		OwnModel: func(config gjson.Result) string {
			if own := openCodeOwnGatewayProvider(ref, gateway, openCodeSnapshotValues(config)); own != "" {
				return own + "/" + ref
			}
			return ""
		},
		KeepProvider: func(config gjson.Result) bool {
			small := config.Get("small_model").String()
			if title := config.Get(openCodeTitleKey); title.Exists() {
				small = openCodeSelection(title.Raw)
			}
			dropProvider = !usesMagpie(small) && !(checkChamber && openChamberOnMagpie())
			return !dropProvider
		},
	}
	if err := f.applyV2(false, &pick); err != nil {
		return err
	}
	if dropProvider {
		return openCodePluginGone(f.path)
	}
	return nil
}

// openCodeSelection is a model as OpenCode 2 has one, raw: "provider/model"
// (#variant), or {providerID, model, variant}, as "provider/model".
func openCodeSelection(raw string) string {
	var s string
	if json.Unmarshal([]byte(raw), &s) == nil {
		return s
	}
	var o struct {
		ProviderID string `json:"providerID"`
		Model      string `json:"model"`
	}
	if json.Unmarshal([]byte(raw), &o) != nil || o.ProviderID == "" || o.Model == "" {
		return ""
	}
	return o.ProviderID + "/" + o.Model
}

// openCodeFromV1 is a provider in OpenCode 1's words, raw, in OpenCode 2's,
// as OpenCode 2.2.6 reads it (packages/core/src/v1/config/migrate.ts,
// migrateStandardProvider): npm is package "aisdk:<npm>", api a baseURL in
// settings, options settings but for headers and body, which are the
// provider's own, and setCacheKey, which is compatibility's
// supportsPromptCacheKey. Of a model it keeps the parts magpie's models
// leave to the user, as openCodeNativeKept reads them: its name, limit and
// variants (openCodeNativeVariant); magpie writes the rest of its models
// whole. What OpenCode 2 doesn't read of OpenCode 1's (id, whitelist,
// blacklist) goes, as it goes there.
func openCodeFromV1(raw []byte) (orderedJSON, bool) {
	var p struct {
		API     string                     `json:"api"`
		Name    json.RawMessage            `json:"name"`
		Env     json.RawMessage            `json:"env"`
		NPM     string                     `json:"npm"`
		Options map[string]json.RawMessage `json:"options"`
		Models  json.RawMessage            `json:"models"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil, false
	}
	out := orderedJSON{}
	if p.Name != nil {
		out = append(out, jsonPair{"name", p.Name})
	}
	if p.Env != nil {
		out = append(out, jsonPair{"env", p.Env})
	}
	if p.NPM != "" {
		out = append(out, jsonPair{"package", "aisdk:" + p.NPM})
	}
	if ck, ok := p.Options["setCacheKey"]; ok {
		out = append(out, jsonPair{"compatibility", map[string]any{"supportsPromptCacheKey": ck}})
	}
	if p.Options != nil || p.API != "" {
		settings := maps.Clone(p.Options)
		if settings == nil {
			settings = map[string]json.RawMessage{}
		}
		for _, k := range []string{"setCacheKey", "headers", "body"} {
			delete(settings, k)
		}
		if p.API != "" {
			b, _ := json.Marshal(p.API)
			settings["baseURL"] = b
		}
		out = append(out, jsonPair{"settings", settings})
	}
	for _, k := range []string{"headers", "body"} {
		if v, ok := p.Options[k]; ok {
			out = append(out, jsonPair{k, v})
		}
	}
	if p.Models != nil {
		ms := orderedJSON{}
		for _, mp := range objectPairs(p.Models) {
			var m struct {
				Name     json.RawMessage `json:"name"`
				Limit    json.RawMessage `json:"limit"`
				Variants json.RawMessage `json:"variants"`
			}
			if json.Unmarshal(mp.v.(json.RawMessage), &m) != nil {
				continue
			}
			e := orderedJSON{}
			if m.Name != nil {
				e = append(e, jsonPair{"name", m.Name})
			}
			if m.Limit != nil {
				e = append(e, jsonPair{"limit", m.Limit})
			}
			if m.Variants != nil {
				list := []any{}
				for _, vp := range objectPairs(m.Variants) {
					var opts map[string]any
					if json.Unmarshal(vp.v.(json.RawMessage), &opts) != nil {
						continue
					}
					list = append(list, openCodeNativeVariant(vp.k, opts))
				}
				e = append(e, jsonPair{"variants", list})
			}
			ms = append(ms, jsonPair{mp.k, e})
		}
		out = append(out, jsonPair{"models", ms})
	}
	return out, true
}
