package agent

// An agent's own telemetry, off (OipsAnthony on X: "agent 管理页面支持给每个
// agent 关闭遥测功能"). An agent with a documented switch for the usage data
// it sends its vendor has it in its settings on the Agents page, which
// writes that switch into the agent's own config. Only agents whose key was
// read in their source or binary:
//
//   - Codex: [analytics] enabled = false in config.toml. "When false,
//     disables OpenAI analytics across Codex product surfaces"; the default
//     statsig metrics exporter follows it (AnalyticsConfigToml and
//     OtelConfigToml in codex-rs/config/src/types.rs, rust-v0.167.0).
//     feedback.enabled is left alone: it takes /feedback away, which only
//     sends when the user asks.
//   - Claude Code: env DISABLE_TELEMETRY and DISABLE_ERROR_REPORTING in
//     settings.json. 2.1.316 takes any value of DISABLE_TELEMETRY as its
//     "no-telemetry" level, from any settings tier, and any value of
//     DISABLE_ERROR_REPORTING keeps its error reports in.
//     CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC would also stop its updates,
//     which is no telemetry.
//   - Gemini CLI and Qwen Code: privacy.usageStatisticsEnabled = false in
//     settings.json (settingsSchema.ts of each, default true).
//   - Crush: options.disable_metrics = true in crush.json (Options in
//     internal/config/config.go, read from its binary; crush also takes
//     DO_NOT_TRACK).
//   - Kimi Code: telemetry = false at the top of config.toml (app.py:
//     telemetry_disabled = not config.telemetry or KIMI_DISABLE_TELEMETRY).
//   - Goose: GOOSE_TELEMETRY_ENABLED: false in config.yaml (the key its
//     `goose configure` telemetry choice writes).
//   - Pi: enableInstallTelemetry = false in settings.json (docs/settings.md:
//     anonymous install/update reporting, default true; its env
//     PI_TELEMETRY wins over the file).
//   - Cline CLI: telemetryOptOut = true in data/settings/global-settings.json
//     (the setting its core reads for every telemetry event; default
//     false).
//
// Antigravity, Droid, OpenCode, Cursor CLI, Hermes and omp have none to
// write: Antigravity's agy keeps it as the Google account's server-side
// setting (its EnableTelemetry field is json:"-"), Droid's are set by an
// organisation, OpenCode's and omp's OpenTelemetry export is opt-in,
// Cursor's privacy mode is the account's, and Hermes sends none.
//
// None is written unless the user turns it off here. Off writes each key
// the file doesn't have and remembers it in the stash; a key the user has
// already written is theirs: one that already turns telemetry off is left
// as it is, and one that keeps it on is never written over (off is
// refused, naming it). On takes out only what magpie wrote, as magpie
// wrote it. Connecting or disconnecting the agent leaves it alone: it is
// about the agent's vendor, not about magpie.
//
// An agent in a WSL distro has its own, in the distro's file (#7460:
// Claude Code's in WSL had none, and Settings › Privacy's switch wrote
// Windows' settings.json instead). Only a distro that runs has it: a
// stopped one's file isn't read, which would start it (#3896), and one
// that stopped since is asked first (wslAgent).

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/extension"
	builtintelemetry "github.com/yetone/magpie/internal/extension/builtin/telemetry"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// TelemetryState is an agent's telemetry as its config has it.
type TelemetryState struct {
	// Off: every key of the switch turns telemetry off
	Off bool `json:"off"`
	// Own: keys the user wrote that keep it off, which on leaves in
	Own []string `json:"own,omitempty"`
	// Keys: what the switch writes, as the file spells it
	Keys []string `json:"keys"`
	// File: the config it writes
	File string `json:"file"`
	// Error: why the config couldn't be read, when it couldn't (the
	// state is then unknown)
	Error string `json:"error,omitempty"`
	// All: Settings › Privacy turns every agent's off, which holds this
	// agent's row (telemetry_all.go); AllError why it couldn't here
	All      bool   `json:"all,omitempty"`
	AllError string `json:"allError,omitempty"`
}

// Telemetry is an agent's switch for its own telemetry.
type Telemetry struct {
	Get func() (TelemetryState, error)
	Set func(off bool) error
	// all: the stash key that says Settings › Privacy's switch for every
	// agent turned this one off (telemetry_all.go)
	all string
}

var errTelemetryUserOff = errors.New("magpie leaves a value of yours as it is (take it out there)")

// telemetryKey is one key of a switch: name as the file spells it (a
// dotted path in JSON; "table.key" in TOML), value what magpie writes, off
// whether a value the file has keeps telemetry off.
type telemetryKey = builtintelemetry.Key

// telemetryFile reads and writes one key of the agent's config. get says
// whether the key is there, and fails on a file that doesn't parse (which
// is unknown, never "not there").
type telemetryFile struct {
	path   string
	format builtintelemetry.Format
	get    func(name string) (v any, has bool, err error)
}

func isFalse(v any) bool { return v == false }
func isTrue(v any) bool  { return v == true }

// anyValue: an environment variable that is set, to anything but empty
func anyValue(v any) bool {
	switch x := v.(type) {
	case string:
		return x != ""
	case nil:
		return false
	}
	return true
}

// telemetrySwitch is the switch over keys in f; stashKey is where what
// magpie wrote is kept.
func telemetrySwitch(at place, f telemetryFile, stashKey string, keys ...telemetryKey) *Telemetry {
	if at.cold {
		return nil
	}
	names := make([]string, len(keys))
	for index, key := range keys {
		names[index] = key.Name
	}
	get := func() (TelemetryState, error) {
		st := TelemetryState{Off: true, Keys: names, File: at.native(f.path)}
		mine := strings.Split(stashShared()[stashKey], ",")
		for _, key := range keys {
			value, has, err := f.get(key.Name)
			if err != nil {
				return TelemetryState{Keys: names, File: st.File}, err
			}
			if !has || !key.Off(value) {
				st.Off = false
				continue
			}
			if !slices.Contains(mine, key.Name) || !key.Matches(value) {
				st.Own = append(st.Own, key.Name)
			}
		}
		return st, nil
	}
	write := func(off bool) error {
		options := builtintelemetry.SettingsOptions{
			Format: f.format, Off: off, Keys: keys, Previous: stashShared()[stashKey], DisplayPath: at.native(f.path),
		}
		var owned string
		err := extension.ApplyLinkedAgentPlan(options, func(options builtintelemetry.SettingsOptions, settings sdk.ActionSettings) (sdk.AgentPlan, error) {
			plan, next, err := builtintelemetry.PlanSettings(options, settings)
			owned = next
			return plan, err
		}, extension.LinkedActionFile{Name: "config", Path: f.path})
		if err != nil {
			return err
		}
		if owned != options.Previous {
			stash(map[string]string{stashKey: owned})
		}
		if off {
			return nil
		}
		st, err := get()
		if err != nil {
			return err
		}
		if st.Off {
			return fmt.Errorf("%s turns it off itself (%s); %w", at.native(f.path), strings.Join(st.Own, ", "), errTelemetryUserOff)
		}
		return nil
	}
	return &Telemetry{Get: get, Set: write, all: stashKey + ".all"}
}

// jsonTelemetryFile is a JSON or JSONC config's keys, by dotted path.
func jsonTelemetryFile(path string) telemetryFile {
	return cachedTelemetryFile(path, "telemetry json", builtintelemetry.JSON)
}

func cachedTelemetryFile(path, tag string, format builtintelemetry.Format) telemetryFile {
	return telemetryFile{
		path: path, format: format,
		get: func(name string) (any, bool, error) {
			lookup, err := filememo.ReadNow(tag, path, format.Decode)
			if errors.Is(err, fs.ErrNotExist) {
				return nil, false, nil
			}
			if err != nil {
				return nil, false, fmt.Errorf("%s: %w", path, err)
			}
			value, has := lookup(name)
			return value, has, nil
		},
	}
}

// codexTelemetry is Codex's: [analytics] enabled = false in config.toml.
func codexTelemetry(at place, path string) *Telemetry {
	f := telemetryFile{
		path: path, format: builtintelemetry.CodexTOML,
		get: func(name string) (any, bool, error) {
			doc, err := codexTopOf(path)
			if err != nil {
				return nil, false, fmt.Errorf("%s: %w", path, err)
			}
			lookup, err := builtintelemetry.Analytics(doc.Analytics)
			if err != nil {
				return nil, false, fmt.Errorf("%s: %w", path, err)
			}
			value, has := lookup(name)
			return value, has, nil
		},
	}
	return telemetrySwitch(at, f, at.key("codex.telemetry"), telemetryKey{Name: "analytics.enabled", Value: false, Off: isFalse})
}

// claudeTelemetry is Claude Code's: its telemetry and error reports off in
// settings.json's env.
func claudeTelemetry(at place, path string) *Telemetry {
	return telemetrySwitch(at, jsonTelemetryFile(path), at.key("claude.telemetry"),
		telemetryKey{Name: "env.DISABLE_TELEMETRY", Value: "1", Off: anyValue},
		telemetryKey{Name: "env.DISABLE_ERROR_REPORTING", Value: "1", Off: anyValue})
}

// usageStatsTelemetry is Gemini CLI's and Qwen Code's: their usage
// statistics off in settings.json.
func usageStatsTelemetry(at place, path, id string) *Telemetry {
	return telemetrySwitch(at, jsonTelemetryFile(path), at.key(id+".telemetry"),
		telemetryKey{Name: "privacy.usageStatisticsEnabled", Value: false, Off: isFalse})
}

// crushTelemetry is Crush's: options.disable_metrics in crush.json.
func crushTelemetry(at place, path string) *Telemetry {
	return telemetrySwitch(at, jsonTelemetryFile(path), at.key("crush.telemetry"),
		telemetryKey{Name: "options.disable_metrics", Value: true, Off: isTrue})
}

// kimiTelemetry is Kimi Code's: telemetry in config.toml.
func kimiTelemetry(at place, path string) *Telemetry {
	return telemetrySwitch(at, cachedTelemetryFile(path, "telemetry toml", builtintelemetry.TOMLTop), at.key("kimi.telemetry"),
		telemetryKey{Name: "telemetry", Value: false, Off: isFalse})
}

// gooseTelemetry is Goose's: GOOSE_TELEMETRY_ENABLED in config.yaml.
func gooseTelemetry(at place, path string) *Telemetry {
	return telemetrySwitch(at, cachedTelemetryFile(path, "telemetry yaml", builtintelemetry.YAMLTop), at.key("goose.telemetry"),
		telemetryKey{Name: "GOOSE_TELEMETRY_ENABLED", Value: false, Off: isFalse})
}

// piTelemetry is Pi's: its install and update reporting in settings.json.
func piTelemetry(at place, path string) *Telemetry {
	return telemetrySwitch(at, jsonTelemetryFile(path), at.key("pi.telemetry"),
		telemetryKey{Name: "enableInstallTelemetry", Value: false, Off: isFalse})
}

// clineTelemetry is Cline CLI's: telemetryOptOut in its global settings.
func clineTelemetry(at place, path string) *Telemetry {
	return telemetrySwitch(at, jsonTelemetryFile(path), at.key("cline.telemetry"),
		telemetryKey{Name: "telemetryOptOut", Value: true, Off: isTrue})
}

// ErrNoTelemetry: the agent has no telemetry switch magpie knows.
var ErrNoTelemetry = errors.New("magpie knows no switch for this agent's telemetry")

// SetTelemetry turns a's own telemetry off, or back to what it was.
func (a *Agent) SetTelemetry(off bool) error {
	if a.Telemetry == nil {
		return ErrNoTelemetry
	}
	// the switch for every agent holds each one's row while it is on
	telemetryAllMu.Lock()
	defer telemetryAllMu.Unlock()
	if s, err := settings.SharedChecked(); err != nil {
		return err
	} else if s.AgentTelemetryOff {
		return ErrTelemetryAllOff
	}
	return a.Telemetry.Set(off)
}
