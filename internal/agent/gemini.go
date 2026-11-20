package agent

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtingemini "github.com/yetone/magpie/internal/extension/builtin/gemini"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// Gemini CLI only speaks Google's own API, so "provider" here is how it
// authenticates: Google sign-in, a Gemini API key, or Vertex AI. The choice
// lives in settings.json (security.auth.selectedType). A Google provider
// added in magpie lends its key through ~/.gemini/.env, which the CLI loads.
//
// Any catalog model works too: the gateway serves the Gemini API, so magpie
// points GOOGLE_GEMINI_BASE_URL at it, uses the gateway token as the API
// key and names the catalog model in model.name.

func gemini(home string) *Agent { return geminiIn(here(home)) }

// geminiIn is Gemini CLI at a place: this machine's home, or a WSL
// distro's (see wsl.go), whose .env names the gateway as the distro
// reaches it, with the key it takes from there.
func geminiIn(at place) *Agent {
	dir := at.join(".gemini")
	path := filepath.Join(dir, "settings.json")
	envPath := filepath.Join(dir, ".env")
	auth := jsonGet(path, "security.auth.selectedType")
	model := jsonGet(path, "model.name")
	base := func() string { v, _ := edit.GetEnvFile(envPath, "GOOGLE_GEMINI_BASE_URL"); return v }
	envKey := func() string { v, _ := edit.GetEnvFile(envPath, "GEMINI_API_KEY"); return v }
	ownKey := func(key string) bool { return ourKey(key) || key == at.gwKey() }
	// routed: the .env names the gateway, or names it at an address it
	// no longer has (a WSL distro's under NAT changes as WSL restarts) with
	// magpie's key: either way what is there is magpie's, never stashed as
	// the user's own
	routed := func() bool {
		return builtingemini.Routed(base(), envKey(), at.gw(), ownKey)
	}
	magpieKey := func() string {
		for _, id := range []string{"google", "gemini"} {
			if p, err := provider.Find(id); err == nil && p.Key != "" {
				return p.Key
			}
		}
		return ""
	}
	files := []extension.LinkedActionFile{
		{Name: builtingemini.EnvFile, Path: envPath},
		{Name: builtingemini.SettingsFile, Path: path},
	}
	restore := func(reset bool) error {
		baseKey, keyKey := at.key("gemini.base_url"), at.key("gemini.api_key")
		authKey, modelKey := at.key("gemini.auth"), at.key("gemini.model")
		saved := peek(baseKey, keyKey, authKey, modelKey)
		options := builtingemini.RestoreOptions{Gateway: at.gw(), OwnKey: ownKey}
		if !reset {
			options.Original = &builtingemini.OriginalSettings{
				Base: saved[baseKey], Key: saved[keyKey], Auth: saved[authKey], Model: saved[modelKey],
			}
		}
		var retired bool
		err := extension.ApplyLinkedAgentPlan(options, func(options builtingemini.RestoreOptions, snapshot sdk.ActionSettings) (sdk.AgentPlan, error) {
			plan, wasRouted, err := builtingemini.PlanRestore(options, snapshot)
			retired = wasRouted
			return plan, err
		}, files...)
		if err == nil && retired {
			forgetValues(saved)
		}
		return err
	}
	unroute := func() error { return restore(false) }
	setModel := func(v string) error {
		if v == "" {
			return restore(true)
		}
		if isMagpie(v) {
			options := builtingemini.RouteOptions{Gateway: at.gw(), Key: at.gwKey(), Model: v, OwnKey: ownKey}
			var original *builtingemini.OriginalSettings
			err := extension.ApplyLinkedAgentPlan(options, func(options builtingemini.RouteOptions, snapshot sdk.ActionSettings) (sdk.AgentPlan, error) {
				plan, before, err := builtingemini.PlanRoute(options, snapshot)
				original = before
				return plan, err
			}, files...)
			if err == nil && original != nil {
				stash(map[string]string{
					at.key("gemini.base_url"): original.Base, at.key("gemini.api_key"): original.Key,
					at.key("gemini.auth"): original.Auth, at.key("gemini.model"): original.Model,
				})
			}
			return err
		}
		if err := unroute(); err != nil {
			return err
		}
		return edit.SetJSON(path, edit.KV{Path: "model.name", Value: v})
	}

	current := func() string {
		if routed() {
			return magpieID
		}
		if base() != "" {
			return "custom"
		}
		switch auth() {
		case "":
			// none chosen: the CLI asks on its first run, as with no CLI at
			// all — the default, so the row folds away with the others
			return ""
		case "gemini-api-key":
			return "api-key"
		case "vertex-ai":
			return "vertex"
		}
		return "google"
	}
	use := func(id string) error {
		if id == magpieID {
			if routed() {
				return nil
			}
			return fmt.Errorf("pick a model via magpie instead; that routes Gemini CLI through the gateway")
		}
		// an auth it doesn't know is refused before magpie is taken out:
		// `magpie gemini <a model it has no option for>` comes here, and
		// unrouted Gemini CLI first, as `magpie snow --help` took magpie
		// out of Snow (#1457)
		switch id {
		case "", "custom", "google", "vertex", "api-key":
		default:
			return fmt.Errorf("%q is neither one of Gemini CLI's models nor an auth (google, api-key, vertex): nothing was changed", id)
		}
		if err := unroute(); err != nil {
			return err
		}
		switch id {
		case "":
			// back to the CLI's own first-run choice
			if err := edit.DelJSON(path, "security.auth.selectedType"); err != nil {
				return err
			}
		case "custom":
			if current() != "custom" {
				return fmt.Errorf("custom means whatever GOOGLE_GEMINI_BASE_URL is already in %s; set it there", envPath)
			}
			return nil
		case "google":
			if err := edit.SetJSON(path, edit.KV{Path: "security.auth.selectedType", Value: "oauth-personal"}); err != nil {
				return err
			}
		case "vertex":
			if err := edit.SetJSON(path, edit.KV{Path: "security.auth.selectedType", Value: "vertex-ai"}); err != nil {
				return err
			}
		case "api-key":
			if err := edit.SetJSON(path, edit.KV{Path: "security.auth.selectedType", Value: "gemini-api-key"}); err != nil {
				return err
			}
			if k := magpieKey(); k != "" && envKey() != k {
				if err := edit.SetEnvFile(envPath, edit.KV{Path: "GEMINI_API_KEY", Value: k}); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unknown provider %q", id)
		}
		return edit.DelEnvFile(envPath, "GOOGLE_GEMINI_BASE_URL")
	}

	return &Agent{
		ID: "gemini", Name: "Gemini CLI", Icon: "geminicli-color", Aliases: []string{"gemini-cli"}, NoNames: true,
		UA:  []string{"geminicli", "gemini-cli"},
		Bin: "gemini", Dir: dir, Path: path,
		Telemetry: usageStatsTelemetry(at, path, "gemini"),
		// its model's default forgets what it had before magpie, its
		// sign-in's default forgets the sign-in: this puts both back
		Unwire: unroute,
		// Gemini CLI is its binary. What it leaves in ~/.gemini stays when it
		// is uninstalled (#230), and Antigravity keeps its folders there too
		// (antigravity, antigravity-cli, config) and reads GEMINI.md (#330),
		// so nothing there says Gemini CLI is here: a row for it would set
		// up a CLI that can't run. The desktop app's PATH has the user's
		// shell's (proc.UserPath).
		detect: func() bool {
			if Taken(dir) {
				return false
			}
			_, err := proc.Look("gemini")
			return err == nil
		},
		Check: func() string {
			if !isMagpie(model()) {
				return ""
			}
			if d := wiringOff("Gemini CLI", envPath, func(k string) (string, bool) { return edit.GetEnvFile(envPath, k) },
				"GOOGLE_GEMINI_BASE_URL", at.gw(), "GEMINI_API_KEY", at.gwKey()); d != "" {
				return d
			}
			if a := auth(); a != "gemini-api-key" {
				return "Gemini CLI signs in with " + orDefault(a) + " rather than magpie's key, so it asks Google directly"
			}
			return ""
		},
		Notice: func() string {
			if Running(`(^|/)gemini( |$)`) {
				return noticeGemini.String()
			}
			return ""
		},
		Fields: []Field{
			{
				Key: "provider", Label: "auth",
				Get: current,
				Set: use,
				Options: func(map[string]string) []Option {
					key := Option{Value: "api-key", Label: "API key", Icon: "gemini-color"}
					switch {
					case magpieKey() != "":
						key.Note = "the Google key from magpie's providers"
					case envKey() != "":
						key.Note = "GEMINI_API_KEY from ~/.gemini/.env"
					default:
						key.Note = "needs GEMINI_API_KEY — add Google Gemini in magpie's providers"
					}
					out := []Option{
						{Value: "google", Label: "Google", Icon: "gemini-color", Note: "Google account · OAuth sign-in"},
						key,
						{Value: "vertex", Label: "Vertex AI", Icon: "googlecloud-color", Note: "Vertex AI · $GOOGLE_CLOUD_PROJECT"},
					}
					switch current() {
					case "custom":
						out = append(out, Option{Value: "custom", Note: hostOf(base()) + " (from .env)"})
					case magpieID:
						out = append(out, Option{Value: magpieID, Label: "magpie", Note: "the local gateway · every provider's models"})
					}
					return out
				},
			},
			{
				Key: "model", Label: "model",
				Get: model,
				Set: setModel,
				Options: func(map[string]string) []Option {
					var ms []catalog.Model
					for _, m := range catalog.Provider("google") {
						if strings.HasPrefix(m.ID, "gemini-") {
							ms = append(ms, m)
						}
					}
					own := group("Gemini CLI", options(ms, ""))
					return withVia(own, "gemini", "")
				},
			},
		},
	}
}

// what gemini says after a change (notice.go)
var (
	noticeGemini = newNotice("Gemini CLI reads its settings at start-up — restart open gemini sessions to see this.")
)
