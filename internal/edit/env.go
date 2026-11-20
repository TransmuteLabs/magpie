package edit

import (
	"strings"

	"github.com/yetone/magpie/internal/lazyre"
)

// dotenv files (KEY=value per line) as Gemini CLI reads from ~/.gemini/.env.

var envLine = lazyre.New(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*?)\s*$`)

// GetEnvFile reads one key from a dotenv file.
func GetEnvFile(path, key string) (string, bool) {
	raw, err := readHeld(path)
	if err != nil {
		return "", false
	}
	return GetEnv(raw, key)
}

// GetEnv reads one key from a dotenv snapshot.
func GetEnv(raw []byte, key string) (string, bool) {
	// A key set twice takes its last value: dotenv readers, Gemini CLI's among
	// them, let a later line override an earlier one.
	lines := splitLines(string(TrimBOM(raw)))
	for i := len(lines) - 1; i >= 0; i-- {
		if m := envLine.FindStringSubmatch(lines[i]); m != nil && m[1] == key {
			if v, ok := unquote(m[2]); ok {
				return v, true
			}
			v := m[2]
			if i := strings.IndexByte(v, '#'); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
			return v, true
		}
	}
	return "", false
}

// SetEnvFile sets keys in a dotenv file, replacing lines in place and
// appending new ones. Other lines are kept byte for byte.
func SetEnvFile(path string, kvs ...KV) error {
	raw, err := Read(path)
	if err != nil {
		return err
	}
	return WriteAtomic(path, PatchEnv(raw, kvs...))
}

// PatchEnv sets keys in a dotenv snapshot, preserving its encoding and line endings.
func PatchEnv(raw []byte, kvs ...KV) []byte {
	unmarked := TrimBOM(raw)
	orig := string(unmarked)
	lines := splitLines(orig)
	for _, kv := range kvs {
		v := toString(kv.Value)
		if strings.ContainsAny(v, " #\"'$\r\n") {
			// Gemini CLI's dotenv keeps backslashes literal inside single quotes.
			if !strings.ContainsAny(v, "'\r\n") {
				v = "'" + v + "'"
			} else {
				v = `"` + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `"`, `\"`) + `"`
			}
		}
		lines = setLine(lines, kv.Path, kv.Path+"="+v, nil, func(l string) (string, bool) {
			if m := envLine.FindStringSubmatch(l); m != nil {
				return m[1], true
			}
			return "", false
		})
	}
	lines = markEnv(lines, orig)
	return append(append([]byte(nil), raw[:len(raw)-len(unmarked)]...), joinLinesLike(lines, orig)...)
}

// DelEnvFile removes keys from a dotenv file, and the file when they were
// all it held (Gemini CLI's .env, made by magpie for its key).
func DelEnvFile(path string, keys ...string) error {
	raw, err := Read(path)
	if err != nil || len(raw) == 0 {
		return err
	}
	body, changed := PatchEnvWithout(raw, keys...)
	if !changed {
		return nil
	}
	if body == nil {
		return Remove(path)
	}
	return WriteAtomic(path, body)
}

// PatchEnvWithout removes dotenv keys from a snapshot. A changed nil result
// means the file has no remaining entries or user comments.
func PatchEnvWithout(raw []byte, keys ...string) ([]byte, bool) {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	var out []string
	changed := false
	unmarked := TrimBOM(raw)
	for _, l := range splitLines(string(unmarked)) {
		if m := envLine.FindStringSubmatch(l); m != nil && drop[m[1]] {
			changed = true
			continue
		}
		out = append(out, l)
	}
	if !changed {
		return raw, false
	}
	// a note magpie put above a key goes with it
	out = markEnv(out, string(unmarked))
	if strings.TrimSpace(strings.Join(withoutNotes(out), "")) == "" {
		return nil, true
	}
	return append(append([]byte(nil), raw[:len(raw)-len(unmarked)]...), joinLines(out)...), true
}
