package agent

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtingoose "github.com/yetone/magpie/internal/extension/builtin/goose"
)

// Goose (Block's goose, its CLI and desktop app alike) reaches magpie as a
// custom provider: a JSON file of its own in custom_providers beside
// config.yaml, named by the provider's id, which goose loads at start-up
// (crates/goose/src/config/declarative_providers.rs). Its schema is
// DeclarativeProviderConfig (crates/goose-providers/src/declarative.rs), and
// each of its models a ModelInfo (crates/goose-provider-types/src/base.rs):
//
//   - context_limit is the window goose's ContextLimitResolver takes for the
//     model before anything else but GOOSE_CONTEXT_LIMIT
//     (crates/goose-provider-types/src/context_limit.rs). Without it goose
//     guesses from its own registry by the model's name, or 128K.
//   - reasoning is what the desktop app's model picker asks before it shows
//     its Thinking Level (fetchModelReasoning, ui/desktop/src/components/
//     settings/models/modelInterface.ts); a model declared without it is
//     taken for one that doesn't think.
//
// dynamic_models false keeps goose to this list rather than the gateway's
// /v1/models (every model, none of their windows), and
// skip_canonical_filtering keeps magpie's order and models goose's registry
// doesn't know.

// gooseProviderID is magpie's file in Goose's custom_providers and the
// provider GOOSE_PROVIDER names.
const gooseProviderID = builtingoose.ProviderID

// gooseKeys are the secrets goose reads for the native providers whose
// models magpie can list, and which goose takes from the environment before
// its keyring (Config::get_secret, crates/goose/src/config/base.rs; the keys
// from crates/goose/src/providers/<name>_def.rs).
var gooseKeys = map[string]string{
	"anthropic":  "ANTHROPIC_API_KEY",
	"openai":     "OPENAI_API_KEY",
	"google":     "GOOGLE_API_KEY",
	"openrouter": "OPENROUTER_API_KEY",
}

// gooseConfigured is the providers other than magpie that the Goose whose
// config.yaml is at cfg has set up: the ones its providers map marks
// configured or that it uses now (set_active_provider, crates/goose/src/
// config/providers.rs), the ones a legacy <name>_configured marker names
// (migrate_provider_config, config/migrations.rs), and a provider whose key
// is in the environment. A key in goose's keyring can't be seen from here;
// such a provider still counts once goose has used it, as each provider it
// is set up with gets an entry. getenv is the environment goose runs in:
// none of a WSL distro's is read.
func gooseConfigured(cfg string, getenv func(string) string) []string {
	set := map[string]bool{}
	if b, err := os.ReadFile(cfg); err == nil {
		var c map[string]any
		if yaml.Unmarshal(b, &c) == nil {
			if ps, ok := c["providers"].(map[string]any); ok {
				for name, v := range ps {
					if e, ok := v.(map[string]any); ok && e["configured"] == true {
						set[name] = true
					}
				}
			}
			for k, v := range c {
				if name, ok := strings.CutSuffix(k, "_configured"); ok && v == true {
					set[name] = true
				}
			}
			for _, k := range []string{"active_provider", "GOOSE_PROVIDER"} {
				if p, ok := c[k].(string); ok && p != "" {
					set[p] = true
				}
			}
		}
	}
	for p, k := range gooseKeys {
		if getenv(k) != "" {
			set[p] = true
		}
	}
	delete(set, gooseProviderID)
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// gooseProviderPath is magpie's custom provider file for the Goose whose
// config.yaml is at cfg.
func gooseProviderPath(cfg string) string {
	return filepath.Join(filepath.Dir(cfg), filepath.FromSlash(builtingoose.CatalogFile))
}

func gooseCatalog(at place) builtingoose.CatalogOptions {
	current := sharedModels("goose")
	models := make([]builtingoose.Model, 0, len(current))
	for _, model := range current {
		models = append(models, builtingoose.Model{ID: model.ID, Context: model.Context})
	}
	return builtingoose.CatalogOptions{URL: at.v1(), Key: gooseKey(at), Models: models}
}

// gooseKey is the key magpie's custom provider hands the Goose at a place:
// one that names Goose (agentKeyAt), since goose's HTTP client sends no
// User-Agent (reqwest's default, crates/goose-providers/src/api_client.rs),
// so the plain key left its requests to "your agent" on Routing and "other"
// in Usage (#1083).
func gooseKey(at place) string { return agentKeyAt("goose", at.gw()) }

func syncGooseProvider(path string, at place) error {
	raw, err := edit.Read(path)
	if err != nil || raw == nil {
		return nil
	}
	return extension.ApplyLinkedAgentPlan(func() builtingoose.CatalogOptions { return gooseCatalog(at) },
		builtingoose.PlanSync, extension.LinkedActionFile{Name: builtingoose.CatalogFile, Path: path})
}

func applyGooseModel(config, catalog, selection string, at place) error {
	options := builtingoose.ModelOptions{Selection: selection}
	if ref, ok := strings.CutPrefix(selection, gooseProviderID+"/"); ok && isMagpie(ref) {
		options.Catalog = func() builtingoose.CatalogOptions { return gooseCatalog(at) }
	}
	files := []extension.LinkedActionFile{{Name: builtingoose.SettingsFile, Path: config}}
	if selection == "" || options.Catalog != nil {
		files = append(files, extension.LinkedActionFile{Name: builtingoose.CatalogFile, Path: catalog})
	}
	return extension.ApplyLinkedAgentPlan(options, builtingoose.PlanModel, files...)
}

// Goose keeps the provider and model it uses in one of two layouts of its
// config.yaml (crates/goose/src/config/providers.rs, migrations.rs). The
// old one is two flat keys, GOOSE_PROVIDER and GOOSE_MODEL. The new one,
// which goose migrates a config to on its next write, is active_provider
// and an entry in the providers map holding that provider's model:
//
//	active_provider: magpie
//	providers:
//	  magpie:
//	    enabled: true
//	    model: zhipu/glm-5.3
//	    configured: true
//
// goose reads active_provider before GOOSE_PROVIDER and the entry's model
// before GOOSE_MODEL (get_active_provider, get_active_model), and once a
// providers map is there its migration deletes the flat keys on the next
// write (cleanup_legacy_provider_keys). So a model set with the flat keys
// alone on a migrated config is never used, and lost.

// gooseActive is the provider and model the Goose config at cfg uses, read
// as goose reads them (the environment aside).
func gooseActive(cfg string) (provider, model string) {
	provider, _ = edit.GetYAMLTop(cfg, "active_provider")
	if provider == "" {
		provider, _ = edit.GetYAMLTop(cfg, "GOOSE_PROVIDER")
	}
	if provider != "" && !strings.Contains(provider, ".") {
		model, _ = edit.GetYAML(cfg, "providers."+provider+".model")
	}
	if model == "" {
		model, _ = edit.GetYAMLTop(cfg, "GOOSE_MODEL")
	}
	return provider, model
}

// gooseModel is the Goose config at cfg's model as provider/model.
func gooseModel(cfg string) string {
	p, m := gooseActive(cfg)
	switch {
	case m == "":
		return ""
	case p == "":
		return m
	}
	return p + "/" + m
}
