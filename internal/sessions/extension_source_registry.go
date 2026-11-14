package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type SessionSourceBinding struct {
	Plugin   string `json:"plugin"`
	Source   string `json:"source"`
	Agent    string `json:"agent"`
	Identity string `json:"identity"`
}

type sourceGrant struct {
	plugin, identity, root, reference string
	requested                         string
	store                             sdk.SessionSourceStore
	descriptor                        sdk.SessionSourceDescriptor
	info                              fs.FileInfo
}

var sourceEpoch atomic.Uint64
var sourceView atomic.Pointer[sessionSourceView]
var sourcePreparing = make(chan struct{}, 1)
var sourceManager = extension.Default
var sourceBeforeRPC func(context.Context) error
var sourceBeforePublish func()
var sourceCancel atomic.Pointer[context.CancelFunc]

func init() {
	extension.OnChange(invalidateSessionSources)
}

func invalidateSessionSources() {
	sourceEpoch.Add(1)
	if cancel := sourceCancel.Load(); cancel != nil {
		(*cancel)()
	}
}

func sourceBindingsPath() string {
	return filepath.Join(appdir.Config(), "session-source-bindings.json")
}

func readSourceBindings() ([]SessionSourceBinding, error) {
	path := sourceBindingsPath()
	root, err := os.OpenRoot(filepath.Dir(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	_, body, err := readSessionInput(context.Background(), root, filepath.Base(path), 64<<10)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var bindings []SessionSourceBinding
	if err := decodeSourceJSON(body, &bindings); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, binding := range bindings {
		identity, err := hex.DecodeString(binding.Identity)
		if !sdk.ValidID(binding.Plugin) || !sdk.ValidID(binding.Source) || !sdk.ValidID(binding.Agent) ||
			!nativeSourceOwner(binding.Agent) || err != nil || len(identity) != sha256.Size ||
			hex.EncodeToString(identity) != binding.Identity || seen[binding.Agent] {
			return nil, errors.New("invalid or duplicate session source binding")
		}
		seen[binding.Agent] = true
	}
	return bindings, nil
}

func BindSessionSourceNative(ctx context.Context, plugin, source, expectedRevision string) error {
	manager := sourceManager()
	manifest, identity, err := sessionMigrationIdentity(ctx, manager, plugin)
	if err != nil {
		return err
	}
	descriptor, ok := manifest.ResolveSessionSource(source)
	if !ok || !nativeSourceOwner(descriptor.Agent) || expectedRevision == "" {
		return errors.New("native source binding requires an existing owner and exact package revision")
	}
	entries, err := manager.List()
	if err != nil || !slices.ContainsFunc(entries, func(entry extension.Installed) bool {
		return !entry.Disabled && entry.Manifest.ID == plugin && entry.Revision == expectedRevision
	}) {
		return extension.ErrActionChanged
	}
	path := sourceBindingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, held := filelock.Lock(path+".lock", filelock.Wait)
	if !held {
		return errors.New("session source binding is busy")
	}
	defer unlock()
	bindings, err := readSourceBindings()
	if err != nil {
		return err
	}
	bindings = slices.DeleteFunc(bindings, func(binding SessionSourceBinding) bool { return binding.Agent == descriptor.Agent })
	bindings = append(bindings, SessionSourceBinding{Plugin: plugin, Source: source, Agent: descriptor.Agent, Identity: identity})
	_, current, err := sessionMigrationIdentity(ctx, manager, plugin)
	if err != nil || current != identity {
		return extension.ErrActionChanged
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(bindings)
	if err != nil {
		return err
	}
	if err := edit.WriteAtomic(path, body); err != nil {
		return err
	}
	invalidateSessionSources()
	return nil
}

func UnbindSessionSourceNative(ctx context.Context, agent, expectedIdentity string) error {
	if !nativeSourceOwner(agent) || expectedIdentity == "" {
		return errors.New("native source release requires its exact binding identity")
	}
	path := sourceBindingsPath()
	unlock, held := filelock.Lock(path+".lock", filelock.Wait)
	if !held {
		return errors.New("session source binding is busy")
	}
	defer unlock()
	bindings, err := readSourceBindings()
	if err != nil {
		return err
	}
	index := slices.IndexFunc(bindings, func(binding SessionSourceBinding) bool {
		return binding.Agent == agent && binding.Identity == expectedIdentity
	})
	if index < 0 {
		return extension.ErrActionChanged
	}
	bindings = slices.Delete(bindings, index, index+1)
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(bindings)
	if err != nil {
		return err
	}
	if err := edit.WriteAtomic(path, body); err != nil {
		return err
	}
	invalidateSessionSources()
	return nil
}

func nativeSourceOwner(agent string) bool {
	switch agent {
	case "claude", "claude-desktop", "codex", "opencode", "zcode", "pi", "raft", "cindy", "omp", "dsh", "cline",
		"qoder", "qoder-cn", "grok", "workbuddy", "workbuddy-ai", "codebuddy", "droid", "cursor", "hermes", "alma", "kimi", "agy", "antigravity":
		return true
	}
	return false
}

func sourceStoreRoots(descriptor sdk.SessionSourceDescriptor, store sdk.SessionSourceStore) ([]string, error) {
	if store.HomeRelativePath != "" {
		home, err := appdir.Home()
		return []string{filepath.Join(home, filepath.FromSlash(store.HomeRelativePath))}, err
	}
	var roots []string
	switch store.NativeRootSlot {
	case "claude.projects":
		if descriptor.Agent != "claude" {
			return nil, errors.New("foreign native session root")
		}
		for _, root := range ClaudeDirs() {
			roots = append(roots, filepath.Join(root, "projects"))
		}
	case "codex.sessions", "codex.archived-sessions":
		if descriptor.Agent != "codex" {
			return nil, errors.New("foreign native session root")
		}
		name := "sessions"
		if store.NativeRootSlot == "codex.archived-sessions" {
			name = "archived_sessions"
		}
		for _, root := range CodexDirs() {
			roots = append(roots, filepath.Join(root, name))
		}
	case "omp.sessions":
		if descriptor.Agent != "omp" {
			return nil, errors.New("foreign native session root")
		}
		roots = ompSessionRoots()
	case "devin.file-sessions":
		if descriptor.Agent != "devin" {
			return nil, errors.New("foreign native session root")
		}
		for _, root := range DevinCLIDirs() {
			roots = append(roots, filepath.Join(root, "sessions"))
		}
	default:
		return nil, errors.New("unknown session source root slot")
	}
	return roots, nil
}

func makeSourceGrant(plugin, identity string, descriptor sdk.SessionSourceDescriptor, store sdk.SessionSourceStore, root string) (sourceGrant, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return sourceGrant{}, err
	}
	opened, err := os.OpenRoot(resolved)
	if err != nil {
		return sourceGrant{}, err
	}
	defer opened.Close()
	info, err := opened.Stat(".")
	if err != nil || !info.IsDir() {
		return sourceGrant{}, errors.New("session source root is not a directory")
	}
	body, err := json.Marshal([]string{appdir.Config(), plugin, identity, descriptor.ID, descriptor.Agent, store.Slot, root, resolved})
	if err != nil {
		return sourceGrant{}, err
	}
	hash := sha256.Sum256(body)
	return sourceGrant{plugin: plugin, identity: identity, descriptor: descriptor, root: resolved, requested: root, store: store,
		reference: hex.EncodeToString(hash[:]), info: info}, nil
}

func extensionSourceFound() []string {
	if _, err := os.Stat(extension.CatalogPath()); err != nil {
		return nil
	}
	entries, err := sourceManager().List()
	if err != nil {
		return nil
	}
	var found []string
	for _, entry := range entries {
		if entry.Disabled {
			continue
		}
		for _, source := range entry.Manifest.SessionSources {
			for _, store := range source.Stores {
				roots, err := sourceStoreRoots(source, store)
				if err != nil {
					continue
				}
				for _, root := range roots {
					info, err := os.Stat(root)
					if err == nil && info.IsDir() && !slices.Contains(found, source.Agent) {
						found = append(found, source.Agent)
					}
				}
			}
		}
	}
	return found
}
