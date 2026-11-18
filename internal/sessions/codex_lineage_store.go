package sessions

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/yetone/magpie/internal/extension"
)

type codexLineageStore struct {
	requested   string
	inputs      *sessionInputSnapshot
	directories []sessionSourceDirectory
	candidates  map[string][]string
}

func (store *codexLineageStore) open() (*os.Root, error) {
	root, err := os.OpenRoot(store.inputs.root)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(".")
	if err != nil || !os.SameFile(store.inputs.info, info) ||
		!sessionStoreModeEqual(store.inputs.info.Mode(), info.Mode()) {
		root.Close()
		return nil, extension.ErrActionChanged
	}
	return root, nil
}

func openCodexLineageStores(ctx context.Context, source Session) (*codexLineageSnapshot, int, string, error) {
	lineage := &codexLineageSnapshot{}
	selected, relative := -1, ""
	for _, home := range CodexDirs() {
		for index, directory := range []string{"sessions", "archived_sessions"} {
			root := filepath.Join(home, directory)
			name, err := filepath.Rel(root, source.Path)
			if err == nil && name != "." && filepath.IsLocal(name) {
				lineage.home, selected, relative = home, index, filepath.ToSlash(name)
				break
			}
		}
		if selected >= 0 {
			break
		}
	}
	if selected < 0 || !sessionInputPath(relative) || codexRolloutID(filepath.Base(relative)) == "" {
		return nil, 0, "", errors.New("Codex sparse rollout is outside its selected managed native store")
	}
	entries := 0
	for _, directory := range []string{"sessions", "archived_sessions"} {
		store := codexLineageStore{requested: filepath.Join(lineage.home, directory)}
		physical, err := filepath.EvalSymlinks(store.requested)
		if errors.Is(err, fs.ErrNotExist) {
			lineage.stores = append(lineage.stores, store)
			continue
		}
		if err != nil {
			return nil, 0, "", err
		}
		root, err := os.OpenRoot(physical)
		if err != nil {
			return nil, 0, "", err
		}
		info, err := root.Stat(".")
		if err == nil {
			store.inputs = &sessionInputSnapshot{root: physical, info: info}
			store.directories, store.candidates, err = scanCodexLineageStore(ctx, root, &entries)
		}
		root.Close()
		if err != nil {
			return nil, 0, "", err
		}
		lineage.stores = append(lineage.stores, store)
	}
	if lineage.stores[selected].inputs == nil {
		return nil, 0, "", extension.ErrActionChanged
	}
	return lineage, selected, relative, nil
}

func scanCodexLineageStore(ctx context.Context, root *os.Root, count *int) ([]sessionSourceDirectory, map[string][]string, error) {
	var directories []sessionSourceDirectory
	candidates := map[string][]string{}
	var visit func(string, int) error
	visit = func(relative string, depth int) error {
		if err := checkCodexLineageAdmission(ctx); err != nil {
			return err
		}
		if depth > 16 {
			return errors.New("Codex lineage directory depth budget exceeded")
		}
		before, err := root.Lstat(relative)
		if err != nil {
			return err
		}
		if !before.IsDir() {
			return errors.New("Codex lineage requires non-symlink directories")
		}
		directory, err := root.Open(relative)
		if err != nil {
			return err
		}
		defer directory.Close()
		opened, err := directory.Stat()
		if err != nil || !os.SameFile(before, opened) || !opened.IsDir() ||
			!sessionStoreModeEqual(before.Mode(), opened.Mode()) {
			return extension.ErrActionChanged
		}
		current := sessionSourceDirectory{path: relative, info: opened}
		var children []string
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries, err := directory.ReadDir(128)
			for _, entry := range entries {
				*count = *count + 1
				if *count > 10000 {
					return errors.New("Codex lineage enumeration budget exceeded")
				}
				name := path.Join(relative, entry.Name())
				if !sessionInputPath(name) {
					return errors.New("invalid Codex lineage path")
				}
				if entry.Type()&fs.ModeSymlink != 0 {
					return errors.New("Codex lineage does not follow nested symlinks")
				}
				if entry.IsDir() {
					current.entries = append(current.entries, entry.Name()+"/")
					children = append(children, name)
				} else if id := codexRolloutID(entry.Name()); id != "" {
					if !entry.Type().IsRegular() {
						return errors.New("Codex lineage requires regular rollout files")
					}
					current.entries = append(current.entries, entry.Name())
					candidates[id] = append(candidates[id], name)
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
		}
		after, err := directory.Stat()
		selected, selectedErr := root.Lstat(relative)
		if err != nil || selectedErr != nil || !selected.IsDir() || !os.SameFile(opened, selected) ||
			!sessionStoreModeEqual(opened.Mode(), selected.Mode()) || !opened.ModTime().Equal(after.ModTime()) {
			return extension.ErrActionChanged
		}
		slices.Sort(current.entries)
		directories = append(directories, current)
		slices.Sort(children)
		for _, child := range children {
			if err := visit(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	err := visit(".", 0)
	for _, names := range candidates {
		slices.Sort(names)
	}
	return directories, candidates, err
}

func (store *codexLineageStore) check(ctx context.Context, entries *int) error {
	if store.inputs == nil {
		if _, err := os.Lstat(store.requested); !errors.Is(err, fs.ErrNotExist) {
			return extension.ErrActionChanged
		}
		return nil
	}
	physical, err := filepath.EvalSymlinks(store.requested)
	if err != nil || physical != store.inputs.root {
		return extension.ErrActionChanged
	}
	if err := store.inputs.check(ctx); err != nil {
		return err
	}
	root, err := os.OpenRoot(physical)
	if err != nil {
		return err
	}
	defer root.Close()
	directories, _, err := scanCodexLineageStore(ctx, root, entries)
	if err != nil {
		return err
	}
	if len(directories) != len(store.directories) {
		return extension.ErrActionChanged
	}
	for index, expected := range store.directories {
		actual := directories[index]
		if expected.path != actual.path || !slices.Equal(expected.entries, actual.entries) ||
			!os.SameFile(expected.info, actual.info) || !sessionStoreModeEqual(expected.info.Mode(), actual.info.Mode()) {
			return extension.ErrActionChanged
		}
	}
	return nil
}
