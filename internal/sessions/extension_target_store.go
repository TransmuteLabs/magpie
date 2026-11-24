package sessions

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type sessionStoreDirectory struct {
	path string
	info os.FileInfo
}

type sessionTargetStore struct {
	descriptor    sdk.SessionDescriptor
	requested     string
	directory     string
	profile       string
	provider      string
	root          string
	prefix        string
	rootInfo      os.FileInfo
	dirs          []sessionStoreDirectory
	configuration *sessionSourceSnapshot
}

func resolveSessionTargetStore(descriptor sdk.SessionDescriptor) (*sessionTargetStore, error) {
	home, err := appdir.Home()
	if err != nil {
		return nil, err
	}
	store := &sessionTargetStore{descriptor: descriptor}
	switch {
	case descriptor.To == "claude" && descriptor.OutputDir == ".claude/projects":
		store.requested = filepath.Join(ClaudeDir(), "projects")
	case descriptor.To == "codex" && descriptor.OutputDir == ".codex/sessions":
		store.requested = filepath.Join(CodexDir(), "sessions")
		store.provider, store.configuration, err = sessionCodexTargetConfiguration(CodexDir())
		if err != nil {
			return nil, err
		}
	case descriptor.To == "omp" && descriptor.OutputDir == ".omp/agent/sessions":
		store.requested, store.profile, err = extensionOMPStore()
		if err != nil {
			return nil, err
		}
	default:
		if !sessionInputPath(descriptor.OutputDir) {
			return nil, errors.New("invalid session output prefix")
		}
		store.requested = filepath.Join(home, filepath.FromSlash(descriptor.OutputDir))
	}
	if !filepath.IsAbs(store.requested) {
		return nil, errors.New("native target must have a host-selected absolute root")
	}
	store.requested = filepath.Clean(store.requested)
	ancestor := store.requested
	var suffix []string
	for {
		info, err := os.Lstat(ancestor)
		if err == nil {
			if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				return nil, errors.New("native target parent is not a directory")
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return nil, err
		}
		suffix = append([]string{filepath.Base(ancestor)}, suffix...)
		ancestor = parent
	}
	anchor, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(anchor)
	if err != nil || !info.IsDir() {
		return nil, errors.New("native target anchor is not a directory")
	}
	store.directory = filepath.Join(append([]string{anchor}, suffix...)...)
	store.root = anchor
	if len(suffix) == 0 {
		store.root = filepath.Dir(anchor)
	}
	prefix, err := filepath.Rel(store.root, store.directory)
	if err != nil || !sessionInputPath(filepath.ToSlash(prefix)) {
		return nil, errors.New("native target has no confined output prefix")
	}
	store.prefix = filepath.ToSlash(prefix)
	root, err := os.OpenRoot(store.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	store.rootInfo, err = root.Stat(".")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(store.prefix, "/")
	missing := false
	for index := range parts {
		name := filepath.Join(parts[:index+1]...)
		var info os.FileInfo
		if !missing {
			info, err = root.Lstat(name)
			if errors.Is(err, fs.ErrNotExist) {
				missing = true
				info = nil
			} else if err != nil {
				return nil, err
			} else if !info.IsDir() {
				return nil, errors.New("native target contains a symlink or non-directory")
			}
		}
		store.dirs = append(store.dirs, sessionStoreDirectory{path: name, info: info})
	}
	return store, nil
}

func (store *sessionTargetStore) selection() string {
	config := "missing"
	if store.configuration != nil {
		config = store.configuration.path + "\x00" + string(store.configuration.hash[:])
	}
	return store.requested + "\x00" + store.directory + "\x00" + store.profile + "\x00" + store.provider + "\x00" + config
}

func (store *sessionTargetStore) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := resolveSessionTargetStore(store.descriptor)
	if err != nil {
		return err
	}
	if current.selection() != store.selection() {
		return extension.ErrActionChanged
	}
	if store.configuration != nil {
		if current.configuration == nil || !os.SameFile(store.configuration.info, current.configuration.info) ||
			!sessionStoreModeEqual(store.configuration.info.Mode(), current.configuration.info.Mode()) {
			return extension.ErrActionChanged
		}
		if err := store.configuration.check(ctx); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(store.root)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil || !os.SameFile(info, store.rootInfo) || !sessionStoreModeEqual(info.Mode(), store.rootInfo.Mode()) {
		return extension.ErrActionChanged
	}
	for _, before := range store.dirs {
		info, err := root.Lstat(before.path)
		if before.info == nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return extension.ErrActionChanged
			}
			continue
		}
		if err != nil || !info.IsDir() || !os.SameFile(before.info, info) || !sessionStoreModeEqual(info.Mode(), before.info.Mode()) {
			return extension.ErrActionChanged
		}
	}
	return ctx.Err()
}

func sessionCodexTargetConfiguration(home string) (string, *sessionSourceSnapshot, error) {
	path := filepath.Join(home, "config.toml")
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "openai", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, err
	}
	snapshot, body, err := readSessionSource(context.Background(), resolved)
	if err != nil {
		return "", nil, err
	}
	var config codexConfig
	if err := toml.Unmarshal(edit.TOMLWithout(body, "projects"), &config); err != nil {
		return "", nil, errors.New("native target Codex config is unreadable")
	}
	provider := config.Provider
	if profile, ok := config.Profiles[config.Profile]; ok && config.Profile != "" && profile.Provider != "" {
		provider = profile.Provider
	}
	if provider == "" {
		provider = "openai"
	}
	return provider, snapshot, nil
}

func sessionStoreModeEqual(first, second fs.FileMode) bool {
	if runtime.GOOS == "windows" {
		return first&0o200 == second&0o200
	}
	return first.Perm() == second.Perm()
}

func extensionOMPProfile() (string, error) {
	profile, set := appdir.LookupEnv("OMP_PROFILE")
	if !set {
		profile = appdir.Getenv("PI_PROFILE")
	}
	profile = strings.TrimSpace(profile)
	if profile == "" || profile == "default" {
		return "", nil
	}
	if !extensionOMPProfileName(profile) {
		return "", errors.New("invalid host OMP profile")
	}
	return profile, nil
}

func extensionOMPConfigRoot() (string, error) {
	home, err := appdir.Home()
	if err != nil {
		return "", err
	}
	return appdir.OMPConfigRoot(home), nil
}

func extensionOMPStore() (string, string, error) {
	config, err := extensionOMPConfigRoot()
	if err != nil {
		return "", "", err
	}
	profile, err := extensionOMPProfile()
	if err != nil {
		return "", "", err
	}
	if profile != "" {
		config = filepath.Join(config, "profiles", profile)
	}
	agent := filepath.Join(config, "agent")
	override := appdir.Getenv("PI_CODING_AGENT_DIR")
	if profile == "" && override == "" && os.Getenv("PI_CODING_AGENT_DIR") != "" {
		return "", "", errors.New("native OMP target requires an absolute PI_CODING_AGENT_DIR")
	}
	if profile == "" && override != "" {
		if !filepath.IsAbs(override) {
			return "", "", errors.New("native OMP target requires an absolute PI_CODING_AGENT_DIR")
		}
		agent = filepath.Clean(override)
	}
	if (runtime.GOOS == "linux" || runtime.GOOS == "darwin") && agent == filepath.Join(config, "agent") {
		if data := appdir.Getenv("XDG_DATA_HOME"); data != "" {
			base := filepath.Join(data, "omp")
			if profile != "" {
				base = filepath.Join(base, "profiles", profile)
			}
			if _, err := os.Stat(base); err == nil {
				return filepath.Join(base, "sessions"), profile, nil
			} else if !errors.Is(err, fs.ErrNotExist) {
				return "", "", err
			}
		}
	}
	return filepath.Join(agent, "sessions"), profile, nil
}

func extensionOMPSessionRoots(existing []string) []string {
	var roots []string
	seen := map[string]bool{}
	add := func(name string) {
		if name == "" {
			return
		}
		name = filepath.Clean(name)
		key := sessionInputKey(name)
		if !seen[key] {
			seen[key] = true
			roots = append(roots, name)
		}
	}
	if selected, _, err := extensionOMPStore(); err == nil {
		add(selected)
	}
	if config, err := extensionOMPConfigRoot(); err == nil {
		add(filepath.Join(config, "agent", "sessions"))
		profiles, _ := filepath.Glob(filepath.Join(config, "profiles", "*", "agent", "sessions"))
		for _, name := range profiles {
			if extensionOMPProfileName(filepath.Base(filepath.Dir(filepath.Dir(name)))) {
				add(name)
			}
		}
	}
	if runtime.GOOS != "windows" {
		if data := appdir.Getenv("XDG_DATA_HOME"); data != "" {
			add(filepath.Join(data, "omp", "sessions"))
			profiles, _ := filepath.Glob(filepath.Join(data, "omp", "profiles", "*", "sessions"))
			for _, name := range profiles {
				if extensionOMPProfileName(filepath.Base(filepath.Dir(name))) {
					add(name)
				}
			}
		}
	}
	for _, name := range existing {
		add(name)
	}
	return roots
}

func extensionOMPProfileName(name string) bool {
	base := strings.ToUpper(strings.Split(name, ".")[0])
	reserved := base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
		len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9'
	return name != "default" && safeOmpProfile.MatchString(name) && !strings.HasSuffix(name, ".") && !reserved
}

func extensionOMPProfileForPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	var roots []struct {
		path  string
		agent bool
	}
	if config, err := extensionOMPConfigRoot(); err == nil {
		roots = append(roots, struct {
			path  string
			agent bool
		}{filepath.Join(config, "profiles"), true})
	}
	if data := appdir.Getenv("XDG_DATA_HOME"); data != "" && runtime.GOOS != "windows" {
		roots = append(roots, struct {
			path  string
			agent bool
		}{filepath.Join(data, "omp", "profiles"), false})
	}
	for _, root := range roots {
		base := root.path
		if resolved, err := filepath.EvalSymlinks(base); err == nil {
			base = resolved
		}
		relative, err := filepath.Rel(base, path)
		if err != nil || !filepath.IsLocal(relative) {
			continue
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if root.agent && len(parts) >= 5 && parts[1] == "agent" && parts[2] == "sessions" && extensionOMPProfileName(parts[0]) {
			return parts[0]
		}
		if !root.agent && len(parts) >= 4 && parts[1] == "sessions" && extensionOMPProfileName(parts[0]) {
			return parts[0]
		}
	}
	return ""
}
