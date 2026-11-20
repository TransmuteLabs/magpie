package agent

// OpenMausBot (milind-soni/OpenMausBot, Apache-2.0, an Electron chat app
// whose bots run the CLIs installed on the computer) keeps its settings in
// config.json in its data folder: $OMB_DATA_DIR, else ~/.openmausbot on
// every system (server/config.ts's DATA_DIR). Each engine a bot can run on
// is an instance of a driver in that file's "instances" map; with none
// saved it runs its default fleet (server/instance-fleets.ts).
//
// Its Claude bots run Claude Code isolated, with only the sign-in keys of
// ~/.claude/settings.json (ANTHROPIC_BASE_URL, ANTHROPIC_AUTH_TOKEN, …)
// carried over (server/drivers/claude.ts's readClaudeAuthSettings), so a
// Claude Code wired to magpie reached the gateway already, but its picker
// listed Claude's own models alone: the models setting env magpie writes
// isn't read. Its Codex bots run the user's ~/.codex as it is, which
// magpie's Codex wiring already covers.
//
// So magpie is an engine of its own there, "magpie", written as
// OpenMausBot's own "Claude endpoint" (server/claude-endpoints.ts's
// newClaudeEndpoint) writes one: a claudeAgent instance with access
// "custom", a Claude home of its own under the data folder, requireApiKey,
// the gateway's address and a key naming OpenMausBot (gateway.TokenFor) as
// ANTHROPIC_BASE_URL, ANTHROPIC_AUTH_TOKEN and ANTHROPIC_API_KEY, and every magpie model in endpointModels, its
// picker. An instance with any of those keys owns its routing: neither the
// user's settings nor a provider switch in OpenMausBot's environment
// reaches it (claudeEnvironment, claudeInstanceOwnsRouting).
//
// A saved map is the whole fleet: one holding magpie's alone would take
// every other engine away. So when the file has none, magpie writes the
// default fleet beside its own, as OpenMausBot itself does when the user
// adds an engine (persistableInstanceConfigs), and off takes the map out
// again while it is still that fleet.
//
// OpenMausBot reads the file when it starts, and writes its whole map of
// engines, as it holds it, when the user changes one in its Settings
// (routes/instances.ts: replaceInstances). So a change is refused while it
// runs, where magpie can tell; a sync writes anyway, as one OpenMausBot
// writes over is written again by the next, or within half a minute by
// KeepOpenMausBotListed. One running on an older list than the file's
// offers a Restart on its Agents row (apprestart.go), which reads it.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/beat"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtinomb "github.com/yetone/magpie/internal/extension/builtin/openmausbot"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// ombInstance is the key of magpie's engine in OpenMausBot's config.json.
const ombInstance = builtinomb.InstanceKey

// ombFleetKey is the stash key that says magpie wrote OpenMausBot's default
// fleet into a file that had none.
const ombFleetKey = "openmausbot.fleet"

// ombProc is OpenMausBot's app as ps lists it: the Mac bundle's
// executable, the Linux one (electron-builder.yml's executableName).
var ombProc = []string{`OpenMausBot\.app/Contents/MacOS/OpenMausBot( |$)`, `^([^ ]*/)?openmausbot( |$)`}

// ombApp is OpenMausBot's macOS bundle (electron-builder.yml's appId).
var ombApp = desktopApp{id: "com.openmausbot.app", names: []string{"OpenMausBot.app"}}

var noticeOMB = newNotice("OpenMausBot reads its engines when it starts and writes back the ones it holds when you change one in it — quit OpenMausBot before changing this, then open it and pick magpie as a bot's engine.")

// errOMBRunning is a change refused while OpenMausBot runs.
var errOMBRunning = errors.New("OpenMausBot writes its engines back over what magpie writes now: quit OpenMausBot, then try again")

// ombRunning says OpenMausBot is open where magpie can tell; on Windows,
// where Running can't, no, and the notice asks to quit it first.
var ombRunning = func() bool {
	return runtime.GOOS != "windows" && Running(ombProc...)
}

func openMausBot(home string) *Agent {
	dir := ombDir(home)
	path := filepath.Join(dir, builtinomb.ConfigFile)
	wired := func() bool { _, ok := edit.GetJSON(path, ombInstance); return ok }
	a := &Agent{
		ID: "openmausbot", Name: "OpenMausBot", Icon: "openmausbot-color", Aliases: []string{"omb", "openmaus", "open-maus-bot"},
		Dir: dir, Path: path,
		detect: func() bool { return isDir(dir) || macBundle(ombApp) || ombWindowsApp() },
		Notice: func() string {
			if wired() && Running(ombProc...) {
				return noticeOMB.String()
			}
			return ""
		},
		ListOnly: true,
		// with no model OpenMausBot would take, the engine stays as it was
		Sync: func() error {
			if !wired() {
				return nil
			}
			return ombChange(path, dir, builtinomb.Sync)
		},
		ListsModels: true,
		Check: func() string {
			if !wired() {
				return ""
			}
			return wiringOff("OpenMausBot", path, func(k string) (string, bool) { return edit.GetJSON(path, ombInstance+".environment."+k) }, "ANTHROPIC_BASE_URL", gateway.URL())
		},
		Fields: []Field{{
			Key: "provider", Label: "provider",
			Get: func() string {
				if wired() {
					return magpieID
				}
				return ""
			},
			Set: func(v string) error {
				if ombRunning() {
					return errOMBRunning
				}
				if v == "" {
					return ombChange(path, dir, builtinomb.Off)
				}
				return ombChange(path, dir, builtinomb.Connect)
			},
			Options: func(map[string]string) []Option {
				return []Option{{Value: magpieID, Label: "magpie", Icon: "magpie", Note: "every magpie model under OpenMausBot's magpie engine; pick it for a bot"}}
			},
		}},
	}
	extensionFenceLocalAtomic(a, []string{path})
	return a
}

// ombDir is OpenMausBot's data folder: $OMB_DATA_DIR (only an absolute
// path, here: OpenMausBot takes it as written), else ~/.openmausbot.
func ombDir(home string) string {
	if d := strings.TrimSpace(appdir.Getenv("OMB_DATA_DIR")); d != "" && filepath.IsAbs(d) {
		return d
	}
	return filepath.Join(home, ".openmausbot")
}

// ombWindowsApp reports whether OpenMausBot's per-user Windows install is
// there (electron-builder's NSIS, perMachine false: the package's name
// under Programs).
func ombWindowsApp() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	d := appdir.Getenv("LOCALAPPDATA")
	return d != "" && isFile(filepath.Join(d, "Programs", "openmausbot", "OpenMausBot.exe"))
}

// ombModels is the magpie models OpenMausBot's picker lists for its magpie
// engine, spelled as Claude Code takes them, a 1M one marked [1m]; an id
// OpenMausBot wouldn't take is left out.
func ombModels() []string {
	mark := claude1MFor("openmausbot")
	out := []string{}
	for _, m := range sharedModels("openmausbot") {
		id := provider.ClaudeSpelled(mark(m.ID))
		if builtinomb.AllowsModel(id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// ombSaved is the models magpie's engine at path lists in OpenMausBot's
// picker, as the file has them.
func ombSaved(path string) []string {
	raw, _ := edit.GetJSON(path, ombInstance+".config.endpointModels")
	var have []string
	json.Unmarshal([]byte(raw), &have)
	return have
}

// ombPart is what OpenMausBot reads of magpie's engine at path when it
// starts, for a Restart while it runs on an older one (AppBehind): the
// gateway, the model first picked and the models listed, not the key.
func ombPart(path string) string {
	url, _ := edit.GetJSON(path, ombInstance+".environment.ANTHROPIC_BASE_URL")
	model, _ := edit.GetJSON(path, ombInstance+".environment.ANTHROPIC_MODEL")
	return url + "\n" + model + "\n" + strings.Join(ombSaved(path), "\n")
}

// ombListStale says magpie's engine at path lists other models than magpie
// does for OpenMausBot. A sync rewrites the list when magpie's changes, but
// OpenMausBot writes back the one it held since it started when the user
// changes an engine in it, and an edit of magpie's engine there keeps the
// model picked alone (claude-endpoints.ts's updateClaudeEndpoint): either
// leaves the picker, and its search, without models picked in magpie
// (maxleebeta, 11-08: Antigravity's weren't found), and no catalog change
// follows to write the list again.
//
// It is not a Check drift: that reads as unwired, and a port change
// rewires only agents without one.
func ombListStale(path string) bool {
	want := ombModels()
	if len(want) == 0 {
		return false
	}
	have := ombSaved(path)
	if len(have) != len(want) {
		return true
	}
	for _, m := range want {
		if !slices.Contains(have, m) {
			return true
		}
	}
	return false
}

// ombKeepEvery is how often KeepOpenMausBotListed looks.
const ombKeepEvery = 30 * time.Second

// KeepOpenMausBotListed writes magpie's models into OpenMausBot's engine
// again, while the gateway serves, when OpenMausBot wrote a list over
// them (ombListStale). The model picked in it stays.
func KeepOpenMausBotListed(ctx context.Context) { keepOMBListed(ctx, ombKeepEvery) }

// keepOMBListed is KeepOpenMausBotListed at an interval the tests can
// shorten; a failure is said once, not every round.
func keepOMBListed(ctx context.Context, every time.Duration) {
	said := ""
	for {
		msg := ""
		if err := ombListedOnce(); err != nil {
			msg = err.Error()
		}
		if msg != "" && msg != said {
			log.Print("OpenMausBot: ", msg)
		}
		said = msg
		select {
		case <-ctx.Done():
			return
		case <-beat.Next(every):
		}
	}
}

// ombListedOnce is one round of KeepOpenMausBotListed.
func ombListedOnce() error {
	home, _ := homes()
	a := openMausBot(home)
	if !a.Wired() || !ombListStale(a.Path) {
		return nil
	}
	return synced(a.Sync)
}

func ombChange(path, dir string, intent builtinomb.SettingsIntent) error {
	options := builtinomb.SettingsOptions{Intent: intent}
	var saved map[string]string
	if intent == builtinomb.Off {
		saved = peek(ombFleetKey)
		options.OwnsDefaultFleet = saved[ombFleetKey] != ""
	} else {
		options.Gateway = gateway.URL()
		options.Key = agentKeyAt("openmausbot", options.Gateway)
		options.ClaudeHome = filepath.Join(dir, "providers", magpieID)
		options.Models = ombModels()
	}
	var addedFleet bool
	file := extension.LinkedActionFile{Name: builtinomb.ConfigFile, Path: path, BeforeWrite: func(string) error {
		if addedFleet {
			return os.MkdirAll(dir, 0o700)
		}
		return nil
	}}
	err := extension.ApplyLinkedAgentPlan(options, func(options builtinomb.SettingsOptions, snapshot sdk.ActionSettings) (sdk.AgentPlan, error) {
		plan, added, err := builtinomb.PlanSettings(options, snapshot)
		addedFleet = added
		return plan, err
	}, file)
	if err == nil {
		if addedFleet {
			stash(map[string]string{ombFleetKey: "1"})
		}
		forgetValues(saved)
	}
	return err
}
