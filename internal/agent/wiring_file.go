package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/steady"
)

type wiringFile struct {
	target string
	info   fs.FileInfo
	body   []byte
	named  bool
}

func (file wiringFile) check(path string) error {
	after, err := readWiringPath(path, file.named)
	if err == nil && file.same(after) {
		return nil
	}
	conflict := fmt.Errorf("agent file %s: %w", path, extension.ErrActionChanged)
	return edit.PreserveOnRollback(path, errors.Join(conflict, err))
}

func (file wiringFile) preserveOnError(path string, err error) error {
	if err != nil {
		if changed := file.check(path); changed != nil {
			return errors.Join(err, changed)
		}
	}
	return err
}

func (file wiringFile) same(other wiringFile) bool {
	if file.target != other.target || !bytes.Equal(file.body, other.body) {
		return false
	}
	if file.info == nil || other.info == nil {
		return file.info == nil && other.info == nil
	}
	return file.info.Mode() == other.info.Mode() && os.SameFile(file.info, other.info)
}

func readWiringFile(path string) (wiringFile, error) {
	return readWiringPath(path, false)
}

func readWiringName(path string) (wiringFile, error) {
	return readWiringPath(path, true)
}

func readWiringPath(path string, named bool) (wiringFile, error) {
	root, stat := path, os.Stat
	if named {
		root, stat = filepath.Dir(path), os.Lstat
	}
	target, err := edit.Target(root)
	if err != nil {
		return wiringFile{}, err
	}
	if named {
		target = filepath.Join(target, filepath.Base(path))
	}
	info, err := stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		target, err = missingWiringTarget(target)
		return wiringFile{target: target, named: named}, err
	}
	if err != nil {
		return wiringFile{}, err
	}
	if named && info.Mode()&fs.ModeSymlink != 0 {
		link, err := os.Readlink(target)
		return wiringFile{target: target, info: info, body: []byte(link), named: true}, err
	}
	if !info.Mode().IsRegular() {
		return wiringFile{}, fmt.Errorf("agent file %s is not a regular file", path)
	}
	body, err := steady.ReadFile(target)
	return wiringFile{target: target, info: info, body: body, named: named}, err
}

func missingWiringTarget(path string) (string, error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		return path, nil
	}
	parent, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) && dir != path && os.IsPathSeparator(dir[len(dir)-1]) {
		dir = dir[:len(dir)-1]
		if _, statErr := os.Lstat(dir); errors.Is(statErr, fs.ErrNotExist) {
			parent, err = missingWiringTarget(dir)
		}
	}
	if err != nil {
		return "", err
	}
	if base == "." || base == ".." {
		return "", fmt.Errorf("can't resolve missing agent settings path %q", path)
	}
	return filepath.Join(parent, base), nil
}

type wiringFileEdit struct {
	path    string
	before  wiringFile
	body    []byte
	unlink  bool
	name    *wiringFile
	private bool
}

func writeWiringFiles(edits ...wiringFileEdit) error {
	for index, change := range edits {
		if change.unlink && (index != len(edits)-1 || change.body != nil) || change.before.named && change.body != nil {
			return fmt.Errorf("agent settings unlink for %q must be the final change", change.path)
		}
		for _, other := range edits[:index] {
			first, second := change.before, other.before
			if first.target == second.target || first.info != nil && second.info != nil && os.SameFile(first.info, second.info) {
				return fmt.Errorf("agent settings paths %q and %q name the same file", change.path, other.path)
			}
		}
	}
	current := slices.Clone(edits)
	check := func() error {
		var err error
		for _, file := range current {
			err = errors.Join(err, file.before.check(file.path))
			if file.name != nil {
				err = errors.Join(err, file.name.check(file.path))
			}
		}
		return err
	}
	if err := check(); err != nil {
		return err
	}
	var published []int
	for index, change := range edits {
		if change.body == nil && !change.unlink {
			continue
		}
		var recorded edit.AtomicPublication
		var err error
		if change.unlink {
			if err = check(); err == nil && change.before.info != nil {
				target := change.before.target
				if change.name != nil {
					target = change.name.target
				}
				err = os.Remove(target)
				if err == nil {
					filememo.Forget()
				}
			}
		} else {
			if change.before.info == nil && change.path != change.before.target {
				err = appdir.Gone(filepath.Dir(change.path))
			}
			if err == nil {
				if change.private {
					err = edit.WritePrivateAtomicRecording(change.before.target, change.body, check, &recorded)
				} else {
					err = edit.WriteAtomicRecording(change.before.target, change.body, check, &recorded)
				}
			}
		}
		if recorded.Info != nil {
			published = append(published, index)
			body := recorded.Body
			if recorded.RetainedTail && len(body) < len(change.before.body) {
				body = append(bytes.Clone(body), change.before.body[len(body):]...)
			}
			current[index].before = wiringFile{target: change.before.target, info: recorded.Info, body: body}
		}
		if err == nil {
			continue
		}
		err = errors.Join(err, check())
		for _, index := range slices.Backward(published) {
			original, written := edits[index], current[index]
			unchanged := func() error { return written.before.check(written.path) }
			var restoreErr error
			if original.before.info != nil {
				restoreErr = edit.RestoreAtomicChecked(original.before.target, original.before.body, unchanged, original.before.info)
			} else if restoreErr = unchanged(); restoreErr == nil {
				restoreErr = os.Remove(written.before.target)
				if restoreErr == nil {
					filememo.Forget()
				}
			}
			if restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore agent settings %s: %w", original.path, restoreErr))
			}
		}
		return err
	}
	return nil
}
