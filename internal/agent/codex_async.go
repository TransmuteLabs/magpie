package agent

// Codex's background compaction (0.163 on, #2490): with
// [features.async_compaction] enabled, Codex sends a compaction_trigger of
// its own while a turn goes on, once the conversation is past
// model_async_compact_threshold_percent of the model's window (in that
// table; one at the top level is not read), and takes the summary in at the
// turn's next step. It starts one only on a provider whose
// capabilities.remote_compaction is "v2", which Codex gives OpenAI's own and
// Azure alone: on magpie's [model_providers.magpie] it never ran until that
// was written there too. magpie's gateway answers the trigger as it does
// Codex's own compaction (codexCompact, or the provider's native one).
//
// What magpie writes is kept in the stash, so a value the user set there
// (by hand, or a value they changed since) is theirs: never written over,
// and left when the setting is turned off.
//
// The table form is read only from Codex 0.163.0-alpha.8 on. Before it,
// every [features] entry Codex doesn't know is a bool, and a table there
// stops Codex from loading its config at all ("invalid type: map, expected
// a boolean": 0.162.1's FeaturesToml, and the Codex app on Windows of
// 多喝热水 on Discord, which wouldn't open). The bare async_compaction =
// true every Codex reads: an older one ignores it, a newer one turns the
// feature on at its own 65%. So the table, which the threshold needs, is
// written only when every Codex that reads this config is known to be new
// enough (codexReaders), and the bare key otherwise; a table magpie wrote
// before is taken back out where it no longer is.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	builtincodex "github.com/yetone/magpie/internal/extension/builtin/codex"
	"github.com/yetone/magpie/internal/lazyre"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	codexAsyncTable   = builtincodex.AsyncCompactionTable
	codexAsyncPercent = builtincodex.AsyncCompactionPercent
	// codexAsyncTableFrom is the first Codex whose [features] takes
	// async_compaction as a table (FeatureToml<AsyncCompactionConfigToml>
	// in codex-rs/features, rust-v0.163.0-alpha.8; alpha.5 has none)
	codexAsyncTableFrom = "0.163.0-alpha.8"
)

// CodexReader is a Codex that reads a config: what it is ("cli", "app",
// "ide", "wsl") and its version, "" when that isn't known.
type CodexReader struct {
	Kind    string `json:"kind"`
	Version string `json:"version,omitempty"`
}

// codexReaders are the Codexes that read the config at at: the CLI on
// PATH, the desktop app's own (its bundled codex-cli on a Mac; the Store
// app's on Windows, whose version magpie can't read; on Linux the
// package's resources/codex, which says its version as the CLI does:
// codex-cli 0.166.0-alpha.5.1 in chatgpt 26.1027), the editor
// extension's, or a WSL distro's CLI. A var so tests can name them.
var codexReaders = func(at place) []CodexReader {
	if at.spell != nil {
		return []CodexReader{{Kind: "wsl", Version: at.version}}
	}
	var out []CodexReader
	if bin, _ := (&Agent{ID: "codex", Bin: "codex"}).cliBin(); bin != "" {
		real, _ := throughShim(bin)
		out = append(out, CodexReader{Kind: "cli", Version: installedVersion(real)})
	}
	app := desktopApps["codex"]
	switch runtime.GOOS {
	case "darwin":
		for _, bundle := range macBundles(app) {
			if v, ok := bundledCodexVersion(bundle, app.id); ok {
				out = append(out, CodexReader{Kind: "app", Version: v})
			}
		}
	case "windows":
		if storeAppVersion(windowsStorePackages(), app.store, app.publisher) != "" {
			out = append(out, CodexReader{Kind: "app"})
		}
	case "linux":
		if dir := app.linuxDir(); dir != "" {
			out = append(out, CodexReader{Kind: "app", Version: installedVersion(filepath.Join(dir, "resources", "codex"))})
		}
	}
	return append(out, extensionCodexReaders(at.home)...)
}

// extensionCodexReaders are the Codexes the editors' OpenAI extension
// (openai.chatgpt) carries, one per installed copy, in VS Code's
// ~/.vscode/extensions, VS Code Server's, and each fork's (Cursor,
// Windsurf, VSCodium, …). The extension ships its codex at
// <ext>/bin/<target>/codex, with the version in the codex-package.json
// beside it ("0.169.0-alpha.11.1" in chatgpt 26.51111 on Linux and
// Windows alike); one whose version can't be read is "" (unknown, not old).
// A copy the editor marked obsolete (.obsolete) is not run any more.
func extensionCodexReaders(home string) []CodexReader {
	var out []CodexReader
	es, err := os.ReadDir(home)
	if err != nil {
		return nil
	}
	for _, e := range es {
		if !strings.HasPrefix(e.Name(), ".") {
			continue
		}
		root := filepath.Join(home, e.Name(), "extensions")
		xs, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		var obsolete map[string]bool
		if b, err := os.ReadFile(filepath.Join(root, ".obsolete")); err == nil {
			json.Unmarshal(b, &obsolete)
		}
		for _, x := range xs {
			n := strings.ToLower(x.Name())
			if !x.IsDir() || !strings.HasPrefix(n, "openai.chatgpt-") || obsolete[x.Name()] {
				continue
			}
			out = append(out, CodexReader{Kind: "ide", Version: extensionCodexVersion(filepath.Join(root, x.Name()))})
		}
	}
	return out
}

// extensionCodexVersion is the version in an extension copy's
// codex-package.json (bin/<target>/, or bin/), "" when none is readable.
func extensionCodexVersion(dir string) string {
	paths, _ := filepath.Glob(filepath.Join(dir, "bin", "*", "codex-package.json"))
	paths = append(paths, filepath.Join(dir, "bin", "codex-package.json"))
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			if m := codexPackageVersionRe.FindSubmatch(b); m != nil {
				return strings.TrimSpace(string(m[1]))
			}
		}
	}
	return ""
}

var codexPackageVersionRe = lazyre.New(`"version"\s*:\s*"([^"]+)"`)

// bundledCodexVersion is the version of the codex a Codex app bundle
// ships (Contents/Resources/codex-cli/codex-package.json, "version":
// "0.163.0-alpha.10.2" in ChatGPT.app 26.1013); ok is false for a bundle
// that is no Codex app, "" for one whose version can't be read.
func bundledCodexVersion(bundle, id string) (v string, ok bool) {
	if b, err := os.ReadFile(filepath.Join(bundle, "Contents", "Resources", "codex-cli", "codex-package.json")); err == nil {
		if m := codexPackageVersionRe.FindSubmatch(b); m != nil {
			return strings.TrimSpace(string(m[1])), true
		}
		return "", true
	}
	b, err := os.ReadFile(filepath.Join(bundle, "Contents", "Info.plist"))
	if err != nil {
		return "", false
	}
	m := plistIDRe.FindSubmatch(b)
	return "", m != nil && strings.TrimSpace(string(m[1])) == id
}

// codexAsyncBlocked are the readers that keep the table out: one older
// than codexAsyncTableFrom, or of no known version. None at all is
// unknown as well: a Codex magpie can't see may read the config.
func codexAsyncBlocked(rs []CodexReader) []CodexReader {
	if len(rs) == 0 {
		return []CodexReader{{Kind: "cli"}}
	}
	var out []CodexReader
	for _, r := range rs {
		if r.Version == "" || Newer(codexAsyncTableFrom, r.Version) {
			out = append(out, r)
		}
	}
	return out
}

// codexAsyncHeld is what kept the table out of this machine's config at
// its last sync, for Settings' row to say; nil when nothing did.
var codexAsyncHeld struct {
	sync.Mutex
	rs []CodexReader
}

// CodexAsyncHeld is the Codexes here that kept the background
// compaction's table, and with it its threshold, out at the last sync:
// one too old for the feature, or one whose version isn't known.
func CodexAsyncHeld() []CodexReader {
	codexAsyncHeld.Lock()
	defer codexAsyncHeld.Unlock()
	return codexAsyncHeld.rs
}

// codexAsyncTableOK says whether the table may be written at at.
func codexAsyncTableOK(at place) bool {
	held := codexAsyncBlocked(codexReaders(at))
	if at.spell == nil {
		codexAsyncHeld.Lock()
		codexAsyncHeld.rs = held
		codexAsyncHeld.Unlock()
	}
	return len(held) == 0
}

// codexAsyncStash is what magpie wrote.
type codexAsyncStash = builtincodex.AsyncCompactionState

// codexAsyncNow is how the config has them: the async_compaction value
// (nil, a bool, or a table) and magpie's provider table, nil when absent.
func codexAsyncNow(path string) (feature any, provider map[string]any, err error) {
	doc, err := codexTopOf(path)
	if err != nil {
		return nil, nil, err
	}
	features, _ := doc.Features.(map[string]any)
	providers, _ := doc.ModelProviders.(map[string]any)
	p, _ := providers[magpieID].(map[string]any)
	return features["async_compaction"], p, nil
}

// codexAsyncSync puts the setting into the Codex config at path, where
// Codex is routed through magpie or not: on is the setting, percent its
// threshold (0 leaves Codex's own). key is the stash's. tableOK: every
// Codex reading the config takes the table (codexAsyncTableOK); without
// it the bare key is written, and a table of magpie's taken out. The
// capability is written for a feature the user turned on in the config
// themselves too: on magpie's provider it would never run without it
// (#2493).
func codexAsyncSync(path, key string, routed, on bool, percent int, tableOK bool) error {
	var prior codexAsyncStash
	if raw := stashShared()[key]; raw != "" {
		json.Unmarshal([]byte(raw), &prior)
	}
	options := builtincodex.AsyncCompactionOptions{Routed: routed, On: on, Percent: percent, TableOK: tableOK, Prior: prior}
	var next codexAsyncStash
	err := extension.ApplyLinkedAgentPlan(options, func(options builtincodex.AsyncCompactionOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
		plan, state, err := builtincodex.PlanAsyncCompaction(options, settings)
		next = state
		return plan, err
	}, extension.LinkedActionFile{Name: builtincodex.GlobalConfigFile, Path: path})
	if err != nil || next == prior {
		return err
	}
	if next == (codexAsyncStash{}) {
		forget(key)
	} else {
		body, _ := json.Marshal(next)
		stash(map[string]string{key: string(body)})
	}
	return nil
}

// CodexAsyncHeldHere is the Codexes on this machine that would keep the
// background compaction's threshold out of its config (one before
// codexAsyncTableFrom, or of no known version), asked now rather than at
// the last sync, which asks only while the setting is on.
func CodexAsyncHeldHere(home string) []CodexReader {
	return codexAsyncBlocked(codexReaders(here(tuneHome(home))))
}

// codexAsyncOwn names what the user has in Codex's config.toml that
// comes before magpie's background compaction threshold, as codexAsyncSync
// leaves it: the feature as a bare key of theirs (true at Codex's own 65%,
// or false), their table spelled inline or as dotted keys, enabled = false
// in it, or a threshold of their own. "" when magpie's threshold is what
// Codex would read.
func codexAsyncOwn(home string) string {
	at := here(tuneHome(home))
	path := filepath.Join(at.codexHome(), "config.toml")
	feature, _, err := codexAsyncNow(path)
	if err != nil || feature == nil {
		return ""
	}
	var st codexAsyncStash
	if raw := stashShared()[at.key("codex.async")]; raw != "" {
		json.Unmarshal([]byte(raw), &st)
	}
	bare := "features.async_compaction"
	switch f := feature.(type) {
	case bool:
		if f && st.Bool {
			return ""
		}
		return bare
	case map[string]any:
		if t, err := edit.GetTOMLTable(path, codexAsyncTable); err != nil || t == nil {
			return bare
		}
		if e, has := f["enabled"]; has {
			if on, _ := e.(bool); !on {
				return codexAsyncTable + ".enabled"
			}
		}
		if p, has := f[codexAsyncPercent]; has {
			if n, _ := p.(int64); st.Percent == 0 || int64(st.Percent) != n {
				return codexAsyncTable + "." + codexAsyncPercent
			}
		}
		return ""
	}
	return bare
}
