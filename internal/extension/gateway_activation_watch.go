package extension

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/fsnotify/fsnotify"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const activationWatchLimit = 128

type activationVnodeWatch interface {
	update(map[string]fs.FileInfo, map[string][]activationMissingPath) bool
	changes() <-chan struct{}
	relevant(map[string][]activationMissingPath) bool
	close()
}

type activationMissingPath struct {
	name      string
	directory bool
}

type activationWatch struct {
	root           string
	filesOnly      bool
	backend        *fsnotify.Watcher
	vnode          activationVnodeWatch
	watched        map[string]fs.FileInfo
	dirs           map[string]bool
	files          map[string]bool
	vnodeTargets   map[string]fs.FileInfo
	missingParents map[string][]activationMissingPath
	added          bool
}

func newActivationWatch(root string) *activationWatch {
	return &activationWatch{
		root: root, filesOnly: runtime.GOOS == "darwin" || runtime.GOOS == "freebsd" || runtime.GOOS == "openbsd" ||
			runtime.GOOS == "netbsd" || runtime.GOOS == "dragonfly",
		watched: make(map[string]fs.FileInfo), dirs: make(map[string]bool), files: make(map[string]bool),
	}
}

func (watch *activationWatch) open(openWatch func() (*fsnotify.Watcher, error)) {
	if watch.root == "" || watch.active() {
		return
	}
	if openWatch == nil {
		if watch.filesOnly {
			watch.vnode = newActivationVnodeWatch()
			if watch.vnode != nil {
				if watch.vnodeTargets == nil {
					watch.vnodeTargets = make(map[string]fs.FileInfo)
					watch.missingParents = make(map[string][]activationMissingPath)
				}
				watch.begin()
				watch.finish()
			}
			return
		}
		openWatch = fsnotify.NewWatcher
	}
	watch.backend, _ = openWatch()
}

func (watch *activationWatch) active() bool {
	return watch.backend != nil || watch.vnode != nil
}

func (watch *activationWatch) close() {
	if watch.vnode != nil {
		watch.vnode.close()
		watch.vnode = nil
		clear(watch.vnodeTargets)
		clear(watch.missingParents)
	}
	if watch.backend != nil {
		_ = watch.backend.Close()
		watch.backend = nil
		clear(watch.watched)
	}
}

func (watch *activationWatch) events() <-chan fsnotify.Event {
	if watch.backend != nil {
		return watch.backend.Events
	}
	return nil
}

func (watch *activationWatch) errors() <-chan error {
	if watch.backend != nil {
		return watch.backend.Errors
	}
	return nil
}

func (watch *activationWatch) vnodeChanges() <-chan struct{} {
	if watch.vnode != nil {
		return watch.vnode.changes()
	}
	return nil
}

func (watch *activationWatch) vnodeRelevant() bool {
	return watch.vnode.relevant(watch.missingParents)
}

func (watch *activationWatch) begin() {
	clear(watch.dirs)
	clear(watch.files)
	clear(watch.vnodeTargets)
	clear(watch.missingParents)
	if watch.backend != nil {
		registered := watch.backend.WatchList()
		slices.Sort(registered)
		for name := range watch.watched {
			if _, exists := slices.BinarySearch(registered, name); !exists {
				delete(watch.watched, name)
			}
		}
	}
	if watch.active() {
		watch.followFile(filepath.Join(watch.root, "catalog.json"))
	}
	watch.added = false
}

func (watch *activationWatch) followConfig(id string) {
	if watch.filesOnly {
		watch.followFile(filepath.Join(watch.root, observerEnablementFile(id)))
	}
	dir := filepath.Join(watch.root, "private", id, "state")
	watch.followFile(filepath.Join(dir, storeStateName(sdk.Scope{Plugin: id}, moduleConfigKey)))
}

func (watch *activationWatch) followFile(name string) {
	if len(watch.files) >= activationWatchLimit {
		return
	}
	watch.files[name] = true
	dir := filepath.Dir(name)
	available := watch.directory(dir, true)
	if !watch.filesOnly {
		return
	}
	if available {
		if info, err := os.Lstat(name); err == nil && info.Mode().IsRegular() {
			watch.track(name, info)
			return
		}
		if watch.vnode != nil {
			watch.missingParents[dir] = append(watch.missingParents[dir], activationMissingPath{name: name})
		}
	}
	watch.remove(name)
}

func (watch *activationWatch) directory(dir string, insideRoot bool) bool {
	if valid, seen := watch.dirs[dir]; seen {
		return valid
	}
	watch.dirs[dir] = false
	parent := filepath.Dir(dir)
	var info fs.FileInfo
	var err error
	if insideRoot {
		if !watch.directory(parent, dir != watch.root) {
			return false
		}
		info, err = os.Lstat(dir)
	} else {
		info, err = os.Stat(dir)
		if (err != nil || !info.IsDir()) && parent != dir {
			watch.directory(parent, false)
		}
	}
	if err != nil || !info.IsDir() {
		if watch.vnode != nil && parent != dir && watch.dirs[parent] {
			watch.missingParents[parent] = append(watch.missingParents[parent], activationMissingPath{name: dir, directory: true})
		}
		return false
	}
	watch.dirs[dir] = true
	if !watch.filesOnly || watch.vnode != nil {
		watch.track(dir, info)
	}
	return true
}

func (watch *activationWatch) track(name string, info fs.FileInfo) {
	if watch.vnode != nil {
		if len(watch.vnodeTargets) < activationWatchLimit {
			watch.vnodeTargets[name] = info
		}
		return
	}
	if previous := watch.watched[name]; previous != nil {
		if os.SameFile(info, previous) {
			return
		}
		watch.remove(name)
	}
	if len(watch.watched) < activationWatchLimit && watch.backend.Add(name) == nil {
		watch.watched[name] = info
		watch.added = true
	}
}

func (watch *activationWatch) remove(name string) {
	if watch.watched[name] != nil {
		_ = watch.backend.Remove(name)
		delete(watch.watched, name)
	}
}

func (watch *activationWatch) finish() bool {
	if watch.vnode != nil {
		return watch.vnode.update(watch.vnodeTargets, watch.missingParents)
	}
	for name := range watch.watched {
		if !watch.dirs[name] && !watch.files[name] {
			watch.remove(name)
		}
	}
	return watch.added
}

func (watch *activationWatch) relevant(event fsnotify.Event) bool {
	name := filepath.Clean(event.Name)
	if _, seen := watch.dirs[name]; seen {
		return event.Op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename|fsnotify.Chmod) != 0
	}
	base := filepath.Base(name)
	observer := filepath.Dir(name) == watch.root && strings.HasPrefix(base, ".observer-") && strings.HasSuffix(base, ".json")
	return watch.files[name] || observer
}
