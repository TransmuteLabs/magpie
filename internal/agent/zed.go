package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtinzed "github.com/yetone/magpie/internal/extension/builtin/zed"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const zedProvider = builtinzed.ProviderPath
const zedModel = builtinzed.ModelPath

// Zed stores API keys in the OS credential store, keyed by the API URL.
var zedCredential = saveZedCredential

// Zed uses ~/.config on macOS, XDG on Linux and Roaming AppData on Windows.
func zed(home, cfg string) *Agent {
	bin := os.Getenv("MAGPIE_ZED_BIN")
	if bin == "" {
		bin = "zed"
	}
	processes := zedProcessNames()
	if custom := appdir.Getenv("MAGPIE_ZED_CONFIG_DIR"); custom != "" {
		return zedAtWith(custom, bin, processes)
	}
	switch runtime.GOOS {
	case "darwin":
		cfg = filepath.Join(home, ".config")
	case "windows":
		cfg = appdir.Getenv("APPDATA")
		if cfg == "" {
			cfg = filepath.Join(home, "AppData", "Roaming")
		}
		return zedAtWith(filepath.Join(cfg, "Zed"), bin, processes)
	}
	return zedAtWith(filepath.Join(cfg, "zed"), bin, processes)
}

func zedAt(dir string) *Agent {
	return zedAtWith(dir, "zed", zedProcessNames())
}

func zedAtWith(dir, bin string, processes []string) *Agent {
	path := filepath.Join(dir, builtinzed.SettingsFile)
	model := pairGet(func(k string) (string, bool) { return edit.GetJSON(path, k) }, zedModel+".provider", zedModel+".model")
	a := &Agent{
		ID: "zed", Name: "Zed", Icon: "zed", Bin: bin, Dir: dir, Path: path, Spelled: prefixed,
		UA: []string{"zed"},
		Notice: func() string {
			if usesMagpie(model()) && Running(processes...) {
				return noticeZed.String()
			}
			return ""
		},
		Sync: func() error {
			return applyZedSettings(path, builtinzed.RefreshCatalog, "")
		},
		Check: func() string {
			if !usesMagpie(model()) {
				return ""
			}
			return wiringOff("Zed", path, func(k string) (string, bool) { return edit.GetJSON(path, zedProvider+"."+k) }, "api_url", gatewayV1())
		},
		Fields: []Field{{
			Key: "model", Label: "model", Get: model,
			Set: func(v string) error {
				return applyZedSettings(path, builtinzed.SelectModel, v)
			},
			Options: func(cur map[string]string) []Option {
				var own []Option
				if v := cur["model"]; v != "" && !usesMagpie(v) {
					own = append(own, Option{Value: v, Icon: modelIcon("", v)})
				}
				return withVia(group("Zed", own), "zed", magpieID+"/")
			},
		}},
	}
	extensionFenceLocalAtomic(a, []string{path, stashPath()})
	return a
}

func applyZedSettings(path string, action builtinzed.Action, value string) error {
	gateway := gatewayV1()
	options := builtinzed.Options{
		Action: action, Value: value, Gateway: gateway,
		Reach: func(address string) string { return zedReachURL(address, gateway) },
		Catalog: func() ([]catalog.Model, int) {
			return sharedModels("zed"), unknownWindow(128000)
		},
	}
	stashKey := "zed:" + path + ":"
	var saved map[string]string
	if action == builtinzed.SelectModel {
		if ref, ok := strings.CutPrefix(value, magpieID+"/"); ok && isMagpie(ref) {
			options.ModelRef = ref
		} else {
			saved = peek(stashKey+"model", stashKey+"provider")
			options.Previous = map[string]string{"model": saved[stashKey+"model"], "provider": saved[stashKey+"provider"]}
		}
	}
	var effects builtinzed.Effects
	err := extension.ApplyLinkedAgentPlan(options, func(options builtinzed.Options, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
		plan, pending, err := builtinzed.Plan(options, settings)
		if err != nil {
			return sdk.AgentPlan{}, err
		}
		effects = pending
		if pending.CredentialURL != "" {
			if err := zedCredential(pending.CredentialURL); err != nil {
				return sdk.AgentPlan{}, fmt.Errorf("configure Zed gateway credential: %w", err)
			}
		}
		return plan, nil
	}, extension.LinkedActionFile{Name: builtinzed.SettingsFile, Path: path})
	if err != nil {
		return err
	}
	if len(effects.Save) > 0 {
		prior := make(map[string]string, len(effects.Save))
		for name, entry := range effects.Save {
			prior[stashKey+name] = entry
		}
		stash(prior)
	}
	if effects.Restored {
		forgetValues(saved)
	}
	return nil
}

func zedProcessNames() []string {
	value := os.Getenv("MAGPIE_ZED_PROCESS_NAMES")
	if value == "" {
		return []string{`(^|/)(zed|zeditor|zed-editor)( |$)`}
	}
	var out []string
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			out = append(out, `(^|/)`+regexp.QuoteMeta(name)+`( |$)`)
		}
	}
	if len(out) == 0 {
		return []string{`(^|/)(zed|zeditor|zed-editor)( |$)`}
	}
	return out
}

// zedReachURL is the address Zed's magpie provider
// reaches the gateway at: its api_url where that is the gateway's, or a
// proxy to it where magpie has answered (#1985: a company's reverse proxy,
// billion-context's http://127.0.0.1:8787/bili/<gateway>), else the
// gateway's own.
func zedReachURL(u, baseURL string) string {
	if u == baseURL {
		return u
	}
	if base, ok := proxyBase(u, baseURL); ok {
		magpieAsked(base)
		if surelyMagpie(u, baseURL) {
			return u
		}
	}
	return baseURL
}

// ZedThrough points Zed's magpie provider (a, Zed) at the gateway through
// prefix, an address a proxy takes the gateway's after (billion-context's
// http://127.0.0.1:8787/bili/), "" for the gateway itself, and saves its
// key for that address in the system's credential store, where Zed looks
// it up by the address. Zed must be on magpie already.
func ZedThrough(a *Agent, prefix string) error {
	if a == nil || a.ID != "zed" {
		return fmt.Errorf("not Zed")
	}
	release, err := a.lockMutation()
	if err != nil {
		return err
	}
	defer release()
	return applyZedSettings(a.Path, builtinzed.ChangeProxy, prefix)
}

// ZedURL is the api_url of Zed's magpie provider (a, Zed), "" without one.
func ZedURL(a *Agent) string {
	u, _ := edit.GetJSON(a.Path, builtinzed.AddressPath)
	return u
}

// what zed says after a change (notice.go)
var (
	noticeZed = newNotice("Restart Zed if it still asks for an API key: magpie has configured its gateway credential in the system credential store.")
)
