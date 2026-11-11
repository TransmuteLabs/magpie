package sessions

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yetone/magpie/internal/appdir"
)

// ompCarrySessionsRoot follows omp's default-profile data migration choice.
// Merely setting XDG_DATA_HOME does not migrate an existing installation.
func ompCarrySessionsRoot() string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if xdg := appdir.Getenv("XDG_DATA_HOME"); xdg != "" {
			root := filepath.Join(xdg, "omp")
			if _, err := os.Stat(root); err == nil {
				return filepath.Join(root, "sessions")
			}
		}
	}
	return filepath.Join(OmpDir(), "sessions")
}

// ompCarryCanonical is omp's equivalent-path resolution: resolve a symlink
// when the whole path exists, otherwise use the cleaned absolute path.
func ompCarryCanonical(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// ompCarryBucket is the native session directory name for a working
// directory. Temp precedes home even when the former lies inside the latter.
func ompCarryBucket(cwd, home, temp string) string {
	cwd = ompCarryCanonical(cwd)
	encode := strings.NewReplacer("/", "-", `\`, "-", ":", "-")
	within := func(root string) (string, bool) {
		if root == "" {
			return "", false
		}
		rel, err := filepath.Rel(ompCarryCanonical(root), cwd)
		if err != nil || filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
			return "", false
		}
		if rel == "." {
			return "", true
		}
		return encode.Replace(rel), true
	}
	if rel, ok := within(temp); ok {
		if rel != "" {
			return "-tmp-" + rel
		}
		return "-tmp"
	}
	if rel, ok := within(home); ok {
		return "-" + rel
	}
	if strings.HasPrefix(cwd, "/") || strings.HasPrefix(cwd, `\`) {
		cwd = cwd[1:]
	}
	return "--" + encode.Replace(cwd) + "--"
}

// ompCarryBlobComplete checks a reusable content-addressed blob and
// refreshes an older one's write-grace period before a new journal refers to
// it. Like omp, it reuses complete hash-named files without re-reading them.
func ompCarryBlobComplete(path string, size int64) (bool, error) {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() {
		return false, fmt.Errorf("omp blob %s is not a regular file", path)
	}
	if st.Size() != size {
		return false, nil
	}
	if now := time.Now(); now.Sub(st.ModTime()) >= time.Minute {
		if err := os.Chtimes(path, now, now); errors.Is(err, os.ErrNotExist) {
			return false, nil
		} else if err != nil {
			return false, err
		}
	}
	return true, nil
}

// ompCarryBlob publishes decoded image bytes in omp's global blob store.
// Its staging name is one omp gc recognizes, and rename never exposes a
// partial blob to a concurrent reader.
func ompCarryBlob(sessionsRoot string, data []byte) (string, error) {
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	dir := filepath.Join(filepath.Dir(sessionsRoot), "blobs")
	path := filepath.Join(dir, hash)
	ref := "blob:sha256:" + hash
	if ok, err := ompCarryBlobComplete(path, int64(len(data))); err != nil {
		return "", err
	} else if ok {
		return ref, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var tmp *os.File
	for {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", err
		}
		var err error
		tmp, err = os.OpenFile(filepath.Join(dir, "."+hash+"."+hex.EncodeToString(suffix[:])+".tmp"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		break
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		// Windows can refuse a rename over a concurrently published file.
		if ok, checkErr := ompCarryBlobComplete(path, int64(len(data))); checkErr == nil && ok {
			return ref, nil
		}
		return "", err
	}
	return ref, nil
}

// writeOmpCarry writes a default-profile omp v3 journal, with a fixed-width
// blank title slot and a single branch of native messages.
func writeOmpCarry(cwd string, items []carryItem, now time.Time) (CarryResult, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return CarryResult{}, err
	}
	id := u.String()
	stamp := carryStamp(now, now)
	home, _ := os.UserHomeDir()
	name := strings.NewReplacer(":", "-", ".", "-").Replace(stamp) + "_" + id + ".jsonl"
	sessionsRoot := ompCarrySessionsRoot()
	path := filepath.Join(sessionsRoot, ompCarryBucket(cwd, home, os.TempDir()), name)

	slot := func(pad string) []byte {
		return jsonLine(o("type", "title", "v", 1, "title", "", "updatedAt", stamp, "pad", pad))
	}
	// writeNew supplies the newline, which is part of the 256-byte slot.
	blank := slot("")
	lines := [][]byte{
		slot(strings.Repeat(" ", 256-len(blank)-1)),
		jsonLine(o("type", "session", "version", 3, "id", id, "timestamp", stamp, "cwd", cwd)),
	}
	parent := ""
	add := func(at time.Time, message ord) {
		entryID := uuid.NewString()
		lines = append(lines, jsonLine(o("type", "message", "id", entryID, "parentId", nilIfEmpty(parent),
			"timestamp", carryStamp(at, now), "message", message)))
		parent = entryID
	}
	messageTime := func(at time.Time) int64 {
		if at.IsZero() {
			at = now
		}
		return at.UnixMilli()
	}
	usage := o("input", 0, "output", 0, "cacheRead", 0, "cacheWrite", 0, "totalTokens", 0,
		"cost", o("input", 0, "output", 0, "cacheRead", 0, "cacheWrite", 0, "total", 0))
	assistant := func(at time.Time, block ord, stop string) {
		add(at, o("role", "assistant", "content", []any{block}, "api", "anthropic-messages", "provider", "anthropic",
			"model", "<synthetic>", "usage", usage, "stopReason", stop, "timestamp", messageTime(at)))
	}
	for _, it := range items {
		switch it.Kind {
		case "user":
			content := []any{}
			for _, block := range it.Content {
				switch block.Type {
				case "text":
					content = append(content, o("type", "text", "text", block.Text))
				case "image":
					data := block.Data
					if len(data) >= 1024 {
						encoding := base64.StdEncoding
						if strings.ContainsAny(data, "-_") {
							encoding = base64.URLEncoding
						}
						if !strings.Contains(data, "=") {
							encoding = encoding.WithPadding(base64.NoPadding)
						}
						decoded, err := encoding.DecodeString(data)
						if err != nil {
							return CarryResult{}, fmt.Errorf("carrying image: %w", err)
						}
						data, err = ompCarryBlob(sessionsRoot, decoded)
						if err != nil {
							return CarryResult{}, fmt.Errorf("carrying image: %w", err)
						}
					}
					content = append(content, o("type", "image", "data", data, "mimeType", block.MimeType))
				}
			}
			add(it.Time, o("role", "user", "content", content, "timestamp", messageTime(it.Time)))
		case "assistant":
			assistant(it.Time, o("type", "text", "text", it.Text), "stop")
		case "call":
			if it.Shell == "" {
				assistant(it.Time, o("type", "text", "text", it.note()), "stop")
				continue
			}
			callID := "call_" + uuid.NewString()
			args := o("command", it.Shell)
			if it.Workdir != "" {
				args = append(args, o("cwd", it.Workdir)...)
			}
			assistant(it.Time, o("type", "toolCall", "id", callID, "name", "bash", "arguments", args), "toolUse")
			out := it.Result
			if !it.Done {
				out = "[no output was recorded]"
			}
			add(it.Time, o("role", "toolResult", "toolCallId", callID, "toolName", "bash",
				"content", []any{o("type", "text", "text", out)}, "isError", it.IsError, "timestamp", messageTime(it.Time)))
		}
	}
	if err := writeNew(path, lines); err != nil {
		return CarryResult{}, err
	}
	return CarryResult{Agent: "omp", ID: id, Path: path}, nil
}
