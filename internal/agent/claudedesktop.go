package agent

// Claude Desktop runs on a third-party gateway in its "3p" deployment mode:
// no Anthropic sign-in, and its Code and Cowork tabs work on any plan (a
// Free account signed in sees only "Get Claude Code" there). The mode is
// claude_desktop_config.json's deploymentMode, in its own folder and in
// Claude-3p beside it; the gateway is a profile in Claude-3p/configLibrary,
// the one _meta.json applies. magpie writes "3p" in both, but Desktop reads
// only Claude-3p's, and not even that one when the profile disables its
// chooser (desktop3p):
//
//	Claude/claude_desktop_config.json      {"deploymentMode":"3p",…}
//	Claude-3p/claude_desktop_config.json   {"deploymentMode":"3p",…}
//	Claude-3p/configLibrary/<id>.json      {"inferenceProvider":"gateway",
//	  "inferenceGatewayBaseUrl":"http://127.0.0.1:3425",
//	  "inferenceGatewayApiKey":"magpie-claude-desktop",
//	  "inferenceGatewayAuthScheme":"bearer",
//	  "disableDeploymentModeChooser":true,"coworkEgressAllowedHosts":["*"],
//	  "toolSearchEnabled":true}
//	Claude-3p/configLibrary/_meta.json     {"appliedId":"<id>",
//	  "entries":[…,{"id":"<id>","name":"magpie"}]}
//
// as CC Switch writes them (claude_desktop_config.rs). The folders are in
// ~/Library/Application Support on macOS, %LOCALAPPDATA% on Windows (the
// MSIX package's LocalCache\Local for a packaged Desktop) and
// $XDG_CONFIG_HOME on Linux, as internal/desktopdir finds them. A
// packaged Desktop gets all of it twice, in the package's folders and the
// real ones: up to 2.x it reads the package's copy, from 2.31226 the real
// %LOCALAPPDATA%\Claude-3p (desktopdir.Sets). With no inferenceModels Desktop lists the
// gateway's /v1/models, which the gateway gives it by ids it keeps (see
// gateway/desktop.go). Desktop reads all this at start-up only.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/desktopdir"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/lazyre"
	"github.com/yetone/magpie/internal/provider"
)

// desktopProfileID is magpie's profile in Desktop's configLibrary: a
// UUID like the ones Desktop makes, its last part "magpie" in hex.
const desktopProfileID = "00000000-0000-4000-8000-6d6167706965"

// desktopPaths are the files and folders magpie writes for Claude Desktop.
type desktopPaths struct {
	key                 string // the stash's keys end with it: "" or ".package"
	dir, dir3p          string // Claude, Claude-3p
	config, config3p    string // their claude_desktop_config.json
	library, meta, prof string // configLibrary, its _meta.json, magpie's profile
	// Desktop's own settings (config.json, its "locale" among them) in its
	// userData signed in with Anthropic (desktopdir's Data: Claude, or
	// %APPDATA%\Claude on Windows) and in Claude-3p, its userData in 3p
	// mode. magpie only ever adds the language to the second.
	app, app3p string
	// mcpFrom is the claude_desktop_config.json whose MCP servers the 3p
	// mode is given (desktopCarryMCP): config, but for the isolated
	// Desktop, whose own file is Claude-Magpie-3p's, Claude's
	mcpFrom string
}

func desktopPathsOf(dir, dir3p string) desktopPaths {
	lib := filepath.Join(dir3p, "configLibrary")
	return desktopPaths{
		dir: dir, dir3p: dir3p,
		config:   filepath.Join(dir, "claude_desktop_config.json"),
		config3p: filepath.Join(dir3p, "claude_desktop_config.json"),
		library:  lib, meta: filepath.Join(lib, "_meta.json"), prof: filepath.Join(lib, desktopProfileID+".json"),
		app: filepath.Join(dir, "config.json"), app3p: filepath.Join(dir3p, "config.json"),
		mcpFrom: filepath.Join(dir, "claude_desktop_config.json"),
	}
}

// desktopDirs are Desktop's Claude and Claude-3p folders on goos: the ones
// magpie writes deploymentMode and its profile into (desktopdir.Find).
func desktopDirs(goos, home string, getenv func(string) string) (string, string) {
	d := desktopdir.Find(goos, home, getenv)
	return d.Mode, d.ThreeP
}

// desktopSets are the sets of Desktop's folders magpie wires: the
// isolated Desktop's alone while it is on (desktopIsolatedOn), else
// desktopUsualSets.
func desktopSets(goos, home string, getenv func(string) string) []desktopPaths {
	if p, ok := desktopIsolatedSet(goos, home, getenv); ok && desktopIsolatedOn() {
		return []desktopPaths{p}
	}
	return desktopUsualSets(goos, home, getenv)
}

// desktopUsualSets are those of every set of Desktop's folders, Find's
// first: for a packaged Desktop the package's and the real ones, each with
// its own stash.
func desktopUsualSets(goos, home string, getenv func(string) string) []desktopPaths {
	var out []desktopPaths
	for _, d := range desktopdir.Sets(goos, home, getenv) {
		p := desktopPathsOf(d.Mode, d.ThreeP)
		p.app = filepath.Join(d.Data, "config.json")
		if d.Package != "" {
			p.key = ".package"
		}
		out = append(out, p)
	}
	return out
}

// DesktopConfigs3p are the claude_desktop_config.json files Claude Desktop
// reads in its 3p mode: there its whole userData is Claude-3p, its MCP
// servers too (%LOCALAPPDATA%\Claude-3p on Windows, and the MSIX package's;
// Claude-3p beside Claude elsewhere). The isolated Desktop's
// Claude-Magpie-3p is not one of them: the library gives it servers of
// its own (DesktopTwinApp).
func DesktopConfigs3p(home string) []string {
	var out []string
	for _, p := range desktopUsualSets(desktopdir.OS, home, os.Getenv) {
		out = append(out, p.config3p)
	}
	return out
}

func claudeDesktop(home string) *Agent {
	sets := desktopdir.Sets(desktopdir.OS, home, os.Getenv)
	ps := desktopSets(desktopdir.OS, home, os.Getenv)
	// its row shows the usual folder's file, the library's too, whichever
	// Desktop magpie wires
	p := desktopUsualSets(desktopdir.OS, home, os.Getenv)[0]
	iso := ps[0].key == desktopIsolatedKey
	return &Agent{
		ID: "claude-desktop", Name: "Claude Desktop", Icon: "claude-color", Aliases: []string{"claude-app"},
		Dir: p.dir, Path: p.config,
		detect: func() bool {
			// on Windows Desktop's uninstall leaves %APPDATA%\Claude
			// behind, its claude_desktop_config.json in it (alien_jambob on
			// X: Desktop uninstalled and still listed), so there only its
			// program says it is here; one still on magpie stays listed
			// too, to be turned off
			if desktopdir.OS == "windows" {
				return desktopInstalled(home) || desktopWiredAny(ps)
			}
			// its own data, the folder magpie writes or Claude-3p
			for _, s := range sets {
				for _, d := range s.All() {
					if isDir(d) {
						return true
					}
				}
			}
			// the isolated Desktop's folder
			return iso && isDir(ps[0].dir3p)
		},
		Notice: func() string {
			if iso {
				if desktopWiredAny(ps) {
					return noticeClaudedesktopIsolated.String()
				}
				return noticeClaudedesktopIsolatedOff.String()
			}
			if desktopWiredAny(ps) {
				return noticeClaudedesktop1.String() + " " + noticeClaudedesktop3.String() + " " + noticeClaudedesktop5.String()
			}
			return noticeClaudedesktop2.String() + " " + noticeClaudedesktop4.String()
		},
		Check: func() string {
			if !desktopWiredAny(ps) {
				return ""
			}
			for _, p := range desktopRead(ps) {
				if msg := desktopCheck(p); msg != "" {
					if len(ps) > 1 {
						msg += " (" + p.dir3p + ")"
					}
					return msg
				}
			}
			return ""
		},
		Gateway: desktopGateway,
		Fields: append([]Field{{
			Key: "provider", Label: "provider",
			Get: func() string {
				if desktopWiredAny(ps) {
					return magpieID
				}
				return ""
			},
			Set: func(v string) error { return desktopApply(home, ps, v) },
			Options: func(map[string]string) []Option {
				return []Option{{Value: magpieID, Label: "magpie", Icon: "magpie",
					Note: "Desktop's third-party gateway: Code and Cowork on magpie's models, no Anthropic sign-in (restart Desktop)"}}
			},
		}, desktopGatewayField(ps, func() error { return desktopApply(home, ps, magpieID) })}, append(desktopTierFields(home, ps), desktopSubagentField(ps), desktopSubagentEffortField(ps))...),
		Sync: func() error { return desktopSyncSettings(ps) },
	}
}

// desktopInstalled reports whether Desktop's program is installed on this
// Windows, whatever folders it has (desktopdir.Installed: Squirrel's
// claude.exe or the MSIX's package folder), or opened yet (#1482): or
// whether the AppX registry has its package, the Store's or the one its
// setup.exe installs, for a package folder not made yet.
func desktopInstalled(home string) bool {
	if desktopdir.Installed(desktopdir.OS, home, os.Getenv) {
		return true
	}
	for _, pkg := range windowsStorePackages() {
		// name_version_architecture_resourceid_publisher: the main
		// package, with no resource id
		f := strings.Split(pkg, "_")
		if len(f) == 5 && f[3] == "" && (f[0] == "Claude" || f[0] == "AnthropicPBC.Claude") {
			return true
		}
	}
	return false
}

// DesktopApp is Claude Desktop as the library knows it, with its file at
// path in dir: on Windows listed only while its program is installed
// (desktopInstalled), as the folder its uninstall leaves behind is no sign
// of it; elsewhere by that folder, as any agent is.
func DesktopApp(home, dir, path string) *Agent {
	a := &Agent{ID: "claude-desktop", Name: "Claude Desktop", Icon: "claude-color", Dir: dir, Path: path}
	if desktopdir.OS == "windows" {
		a.detect = func() bool { return desktopInstalled(home) }
	}
	return a
}

// desktopRead are the sets of Desktop's folders whose wiring counts: the
// one whose Claude-3p Desktop ran in last, which is where it reads
// deploymentMode and the gateway from, or every set when it has run in
// none. Magpie writes both of a packaged Desktop's sets, not knowing the
// build: 2.31226's MSIX keeps Claude-3p out of the package
// (ExcludedDirectory), so the package's copy is one Desktop no longer
// reads, and one gone from there (emptied by an update) isn't Desktop off
// magpie (dumplings on Discord: "配置已变更" while Desktop ran on magpie).
func desktopRead(ps []desktopPaths) []desktopPaths {
	var newest time.Time
	var at []desktopPaths
	for _, p := range ps {
		if t := desktopdir.LastRun(p.dir3p); t.After(newest) {
			newest, at = t, []desktopPaths{p}
		}
	}
	if at == nil {
		return ps
	}
	return at
}

// desktopCheck is what keeps one set of Desktop's folders off magpie's
// gateway, "" for nothing.
func desktopCheck(p desktopPaths) string {
	meta, err := desktopReadFields(p.meta)
	if err != nil {
		return err.Error()
	}
	if err := desktopHybridCheck(meta); err != nil {
		return err.Error()
	}
	prof := desktopProfilePath(p, desktopString(meta["appliedId"]))
	if prof == "" {
		return desktopOtherProfile
	}
	profile, err := desktopReadFields(prof)
	if err != nil {
		return err.Error()
	}
	if desktopString(profile["inferenceProvider"]) != "gateway" ||
		prof != p.prof && !desktopIsOurGateway(desktopString(profile["inferenceGatewayBaseUrl"])) {
		return desktopOtherProfile
	}
	if err := desktopProfileCheck(profile); err != nil {
		return err.Error()
	}
	saved, err := desktopReadFields(p.config3p)
	if err != nil {
		return err.Error()
	}
	if !desktop3p(profile, saved) {
		return "Claude Desktop's deploymentMode (Claude-3p/claude_desktop_config.json) is 1p now, so it signs in with Anthropic rather than using magpie"
	}
	gw := desktopGateway()
	want := desktopKey(gw)
	return wiringOff("Claude Desktop", prof, func(k string) (string, bool) {
		raw, ok := profile[k]
		v := desktopString(raw)
		if k != "inferenceGatewayApiKey" {
			return v, ok
		}
		// Desktop seals the key at rest (desktopOpen): the one it opens
		// to is the one it sends; one magpie can't open is unknown, and
		// not said to be changed
		open, known := desktopOpen(k, v)
		if !known {
			if mark := stashShared()[desktopKeyMark+p.key]; prof == p.prof && mark != "" && mark != desktopValueHash(want) {
				return "sealed for the previous gateway key", true
			}
			return want, true
		}
		if prof != p.prof && desktopKeyTaken(open, want) {
			return want, true // a key of magpie's other than Desktop's own
		}
		return open, ok
	}, "inferenceGatewayBaseUrl", gw, "inferenceGatewayApiKey", want)
}

func desktop3p(profile, saved map[string]json.RawMessage) bool {
	if desktopModeCheck(profile) != nil {
		return false
	}
	return desktopString(profile["deploymentMode"]) == "3p" ||
		bytes.Equal(profile["disableDeploymentModeChooser"], []byte("true")) ||
		desktopString(saved["deploymentMode"]) != "1p"
}

// desktopKeyTaken: the gateway takes v where magpie gives Desktop want,
// as keyHolds says, or v is a key of magpie's own (gateway.Token, or one
// naming an agent) where want is one too: the gateway takes those alike.
func desktopKeyTaken(v, want string) bool {
	return keyHolds(v, want) || gatewayKey(want) && v != "" && gatewayKey(v)
}

// desktopAppliedGateway is the configLibrary profile Desktop applies when
// it is not magpie's own but a gateway profile for magpie's gateway, ""
// for none. Desktop's "Import configuration" makes one: it adds the file
// it is given as a new entry under an id of its own (configLibrary's
// createConfig, a random UUID) and writes the file's settings into it, so
// magpie's profile exported on one computer and imported on another is
// applied there under another id, with the connectors (managedMcpServers)
// and settings the user made in it (aa86222 on Discord: 配置已变更 while
// Desktop ran on magpie). It is the user's profile: magpie keeps it
// applied and sets nothing in it but a key the gateway refuses.
func desktopAppliedGateway(p desktopPaths) string {
	meta, err := desktopReadFields(p.meta)
	if err != nil || desktopHybridCheck(meta) != nil {
		return ""
	}
	path := desktopProfilePath(p, desktopString(meta["appliedId"]))
	if path == "" || path == p.prof {
		return ""
	}
	profile, err := desktopReadFields(path)
	if err == nil && desktopIsGatewayProfile(profile) {
		return path
	}
	return ""
}

// desktopApply turns Desktop's wiring on for v == magpieID, off for "",
// or writes it again where the address or key changed.
func desktopApply(home string, ps []desktopPaths, v string) error {
	ps = slices.Clone(ps)
	src := desktopLocaleFrom(ps)
	for index := range ps {
		if src != "" {
			ps[index].app = src
		}
	}
	change := desktopConnect
	if v == "" {
		change = desktopDisconnect
	}
	if err := desktopUpdate(ps, change); err != nil {
		return err
	}
	// its Code tab is Claude Code, told what Desktop's ids for
	// magpie's models can do while that one runs on magpie
	desktopClaudeSync(home)
	// and a running distro's, which a Code tab session with
	// WSL as its host runs on (a stopped one is written on
	// the round after it starts)
	for _, a := range wslAgents() {
		if strings.HasPrefix(a.ID, "claude@wsl:") && a.Sync != nil {
			_ = a.Sync()
		}
	}
	return nil
}

// desktopTierFields pick the model each of Claude Code's tiers runs on in
// Desktop's Code tab, its subagents' sonnet, haiku or opus among them
// (gateway.DesktopTiers). Unset, the chat's model runs it, or, for a chat
// on a Claude model, its provider's model of the tier: another provider's
// Claude model, or a Claude model beside a chat on another vendor's, is
// never used unpicked. Desktop reads them from the
// gateway's /v1/models when it starts, and a pick's behavesAs from its
// profile's inferenceModels (desktopSyncModels), also at start-up.
func desktopTierFields(home string, ps []desktopPaths) []Field {
	var fields []Field
	for _, tier := range gateway.DesktopTierNames {
		fields = append(fields, Field{
			Key: tier, Label: tier, Quiet: true,
			Get: func() string { return gateway.DesktopTiers()[tier] },
			Set: func(v string) error {
				v = gateway.DesktopCatalogID(v)
				if v != "" && !isMagpie(v) {
					return fmt.Errorf("%s: %q is not a model magpie serves", tier, v)
				}
				if err := gateway.SetDesktopTier(tier, v); err != nil {
					return err
				}
				// the profile's list of models, whose behavesAs a pick
				// has, and the window the Code tab's Claude Code holds it
				// to (claudeBudget's behaves)
				err := desktopSyncSettings(ps)
				if desktopWiredAny(ps) {
					desktopClaudeSync(home)
				}
				return err
			},
			Options: func(map[string]string) []Option {
				if !desktopWiredAny(ps) {
					return nil
				}
				return viaMagpie("claude-desktop", "")
			},
		})
	}
	return fields
}

// desktopSubagentField picks the model every subagent turn in Desktop's
// Code tab and Cowork runs on, whatever its lead or tier asked for. It
// writes nothing to Desktop's config, which has no key for it, and the
// Claude Code it runs ignores CLAUDE_CODE_SUBAGENT_MODEL in ~/.claude
// while Desktop manages its provider: it is magpie's setting
// (settings.DesktopSubagentModel), which the gateway reads on each
// request (gateway desktopSubagentPick), so it is Live and a change needs
// no restart of Desktop. Labelled as Codex's "subagent model", it gets the
// same square on the Agents page. A Desktop alias picked is kept by its
// catalog id.
func desktopSubagentField(ps []desktopPaths) Field {
	return Field{
		Key: "subagent_model", Label: "subagent model", Quiet: true, Live: true,
		Get: provider.DesktopSubagentModel,
		Set: func(v string) error {
			return provider.SetDesktopSubagentModel(gateway.DesktopCatalogID(v))
		},
		Options: func(map[string]string) []Option {
			if !desktopWiredAny(ps) {
				return nil
			}
			return viaMagpie("claude-desktop", "")
		},
	}
}

// Desktop's subagent effort is a live gateway setting, like its subagent
// model. With no model picked, a subagent may use any of Desktop's models,
// so all levels are offered and the gateway fits each to the actual model.
func desktopSubagentEffortField(ps []desktopPaths) Field {
	return Field{
		Key: "subagent_effort", Label: "subagent effort", Quiet: true, Live: true,
		Get: provider.DesktopSubagentEffort,
		Set: provider.SetDesktopSubagentEffort,
		Options: func(map[string]string) []Option {
			if !desktopWiredAny(ps) {
				return nil
			}
			target := provider.DesktopSubagentModel()
			if target == "" {
				return static(provider.MemberEfforts...)
			}
			for _, e := range provider.Served() {
				if e.ID != target {
					continue
				}
				if provider.Reasonless(e.Provider.ID, e.Model) {
					return nil
				}
				var levels []string
				for _, level := range e.Efforts {
					if slices.Contains(provider.MemberEfforts, level) {
						levels = append(levels, level)
					}
				}
				if len(levels) > 0 {
					return static(levels...)
				}
			}
			return static(provider.MemberEfforts...)
		},
	}
}

// desktopWired: _meta.json lists magpie's profile.
func desktopWired(p desktopPaths) bool {
	fields, err := desktopReadFields(p.meta)
	if err != nil {
		return false
	}
	entries, err := desktopMetaEntries(fields)
	return err == nil && slices.ContainsFunc(entries, desktopOurs)
}

// desktopWiredAny: any set of Desktop's folders is on magpie.
func desktopWiredAny(ps []desktopPaths) bool { return slices.ContainsFunc(ps, desktopWired) }

// The stash keeps what Desktop had before magpie: each file's
// deploymentMode ("" when it had none), the profile applied, and the
// files and folders magpie made, so switching off puts all of it back.
// It also keeps magpie's profile as it was when switched off, the
// settings the user made in it from Desktop's window among them, so
// switching on again brings them back (#2035).
// A package's folders keep theirs under the same keys ending in ".package".
const (
	desktopStashed = "claude-desktop.on"
	desktopMode    = "claude-desktop.mode"
	desktopMode3p  = "claude-desktop.mode3p"
	desktopApplied = "claude-desktop.applied"
	desktopMade    = "claude-desktop.made"
	desktopProfile = "claude-desktop.profile"
	desktopLocale  = "claude-desktop.locale"
	desktopImport  = "claude-desktop.import"
	desktopSearch  = "claude-desktop.tool-search"
	desktopLong    = "claude-desktop.prefer-1m"
	desktopSSH     = "claude-desktop.ssh-hosts"
	desktopMCP     = "claude-desktop.mcp"
	desktopMCPSeen = "claude-desktop.mcp-seen"
)

var desktopStashKeys = []string{desktopStashed, desktopMode, desktopMode3p, desktopModes, desktopApplied, desktopMade, desktopProfile, desktopLocale, desktopImport, desktopSearch, desktopLong, desktopSSH, desktopMCP, desktopMCPSeen, desktopModelsMark, desktopDiscoveryMark, desktopKeyMark, desktopImported}

// desktopImportOn is Desktop's own "Claude.ai data import" policy
// (claudeAiImport, a 3p-only key of Desktop 1.28929 and later) turned on,
// with its banner shown only when there is something to import.
var desktopImportOn = map[string]any{"enabled": true, "bannerBehavior": "detect"}

func desktopSameJSON(a, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// desktopLocaleName is a language Desktop takes for its "locale": the
// names of its catalogs (lit in Desktop 2.7032's main process,
// /^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*\.json$/).
var desktopLocaleName = lazyre.New(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)

// desktopLocaleFrom is the config.json, of every set's signed-in
// userData, that names a language: the first set's (where Desktop runs)
// before the others, so a packaged Desktop's language reaches the real
// Claude-3p too, which 2.31226 reads. "" for none.
func desktopLocaleFrom(ps []desktopPaths) string {
	for _, p := range ps {
		if loc, ok := edit.GetJSON(p.app, "locale"); ok && desktopLocaleName.MatchString(loc) {
			return p.app
		}
	}
	return ""
}

// desktopIsOurGateway: u is magpie's gateway, by its port on this machine
// (127.0.0.1, localhost or ::1, with or without /v1).
func desktopIsOurGateway(u string) bool {
	a, err := url.Parse(strings.TrimSpace(u))
	if err != nil {
		return false
	}
	if desktopOurAddress(a) {
		return true
	}
	if a.Port() == "" {
		return false
	}
	ours, err := url.Parse(gateway.URL())
	if err != nil || a.Port() != ours.Port() {
		return false
	}
	h := a.Hostname()
	if h == ours.Hostname() || h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// what claudedesktop says after a change (notice.go)
var (
	noticeClaudedesktop1 = newNotice("Claude Desktop reads this at start-up — quit and reopen it to run on magpie (Code and Cowork, no Anthropic sign-in).")
	noticeClaudedesktop2 = newNotice("Claude Desktop reads this at start-up — quit and reopen it to sign in with Anthropic again.")
	// Desktop's 3p mode runs in a userData of its own (Claude-3p, chosen in
	// its main process before the window opens), so the chats, projects and
	// Code sessions of the Anthropic sign-in in Claude/ aren't shown there.
	// magpie never touches Claude/'s data; off brings them back
	// (xgddm88 on X: 之前的工程和会话记录全没了).
	noticeClaudedesktop3 = newNotice("Chats and projects from your Anthropic sign-in stay in Desktop's own profile, not shown on magpie; nothing is deleted, and turning this off brings them back.")
	noticeClaudedesktop4 = newNotice("Your chats and projects from before are back there.")
	// and how to go on with them on magpie (Jacky_yyyyyyyy on X): Desktop's
	// own importer, which magpie's profile turns on (desktopSeedImport).
	// With claudeAiImport.enabled, 2.7032 and 4.16336 offer it in 3p mode
	// under Settings → Import: Claude.ai's chats and projects (a sign-in
	// to Claude.ai there, or its data export zip) and the Code and Cowork
	// sessions of the signed-in profile, copied in under new ids, the
	// originals left alone.
	noticeClaudedesktop5 = newNotice("To go on with them on magpie, open Settings → Import in Desktop: it can copy in your Claude.ai chats and projects and this computer's earlier sessions, and leaves the originals as they are.")
	// the isolated Desktop (#5671): magpie's is its own app, and the
	// user's Claude.app stays signed in with Anthropic
	noticeClaudedesktopIsolated    = newNotice("Open Claude Magpie from Applications to use Claude Desktop on magpie. Your own Claude Desktop stays signed in with Anthropic.")
	noticeClaudedesktopIsolatedOff = newNotice("Quit Claude Magpie if it is open: it no longer runs on magpie.")
)
