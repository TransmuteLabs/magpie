package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/steady"
)

type wiringFile struct {
	target string
	info   fs.FileInfo
	body   []byte
}

func (file wiringFile) check(path string) error {
	after, err := readWiringFile(path)
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
	target, err := edit.Target(path)
	if err != nil {
		return wiringFile{}, err
	}
	info, err := os.Stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return wiringFile{target: target}, nil
	}
	if err != nil {
		return wiringFile{}, err
	}
	if !info.Mode().IsRegular() {
		return wiringFile{}, fmt.Errorf("agent file %s is not a regular file", path)
	}
	body, err := steady.ReadFile(target)
	return wiringFile{target: target, info: info, body: body}, err
}

type wiringFileEdit struct {
	path   string
	before wiringFile
	body   []byte
}

func writeWiringFiles(edits ...wiringFileEdit) error {
	for index, change := range edits {
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
		}
		return err
	}
	if err := check(); err != nil {
		return err
	}
	var published []int
	for index, change := range edits {
		if change.body == nil {
			continue
		}
		var recorded edit.AtomicPublication
		err := edit.WriteAtomicRecording(change.before.target, change.body, check, &recorded)
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
