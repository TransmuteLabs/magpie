package extension

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

func RestoreOwnedActionSnapshots(ctx context.Context, rootPath string, original []ActionSettingFile, owned ...[]ActionSettingFile) error {
	if ctx == nil || !filepath.IsAbs(rootPath) || len(original) > MaxActionFiles || len(owned) > 2 {
		return errors.New("invalid owned action recovery")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer root.Close()
	unlock, err := actionLock(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
	type restoreFile struct {
		name   string
		from   actionFile
		before actionFile
	}
	var changes []restoreFile
	seen, total := map[string]bool{}, 0
	for _, snapshot := range original {
		total += len(snapshot.Body)
		before := actionFile{Body: snapshot.Body, Mode: fs.FileMode(snapshot.Mode), Exists: snapshot.Exists}
		if !actionPath(snapshot.Path) || seen[snapshot.Path] || snapshot.Mode & ^uint32(0o777) != 0 ||
			total > MaxActionBytes || snapshot.BeforeHash != actionHash(before) ||
			!snapshot.Exists && (len(snapshot.Body) != 0 || snapshot.Mode != 0) {
			return errors.New("invalid original action recovery snapshot")
		}
		seen[snapshot.Path] = true
		current, err := actionRead(root, snapshot.Path)
		if err != nil {
			return err
		}
		if actionEqual(current, before) {
			continue
		}
		matched := false
		for _, candidates := range owned {
			for _, candidate := range candidates {
				after := actionFile{Body: candidate.Body, Mode: fs.FileMode(candidate.Mode), Exists: candidate.Exists}
				if candidate.Path == snapshot.Path && candidate.BeforeHash == actionHash(after) && actionEqual(current, after) {
					matched = true
				}
			}
		}
		if !matched {
			return ErrActionChanged
		}
		changes = append(changes, restoreFile{name: snapshot.Path, from: current, before: before})
	}
	write := func(name string, value actionFile) error {
		if !value.Exists {
			err := root.Remove(name)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			return actionSync(root, path.Dir(name))
		}
		if _, err := actionParents(root, name); err != nil {
			return err
		}
		return actionWrite(root, name, value.Body, value.Mode)
	}
	var changed []restoreFile
	for _, change := range changes {
		err = ctx.Err()
		if err == nil {
			var current actionFile
			current, err = actionRead(root, change.name)
			if err == nil && !actionEqual(current, change.from) {
				err = ErrActionChanged
			}
		}
		if err == nil {
			changed = append(changed, change)
			err = write(change.name, change.before)
		}
		if err != nil {
			break
		}
	}
	if err == nil {
		return nil
	}
	var rollback error
	for index := len(changed) - 1; index >= 0; index-- {
		change := changed[index]
		current, readErr := actionRead(root, change.name)
		if readErr != nil {
			rollback = errors.Join(rollback, readErr)
		} else if actionEqual(current, change.before) {
			rollback = errors.Join(rollback, write(change.name, change.from))
		} else if !actionEqual(current, change.from) {
			rollback = errors.Join(rollback, ErrActionChanged)
		}
	}
	return errors.Join(err, rollback)
}

func CloneActionSnapshots(files []ActionSettingFile) []ActionSettingFile {
	out := make([]ActionSettingFile, len(files))
	for index, file := range files {
		out[index] = file
		out[index].Body = append([]byte(nil), file.Body...)
	}
	return out
}

func ActionSnapshotsMatch(ctx context.Context, rootPath string, expected []ActionSettingFile) error {
	if ctx == nil || !filepath.IsAbs(rootPath) || len(expected) > MaxActionFiles {
		return errors.New("invalid owned action snapshots")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer root.Close()
	unlock, err := actionLock(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
	total := 0
	for _, snapshot := range expected {
		total += len(snapshot.Body)
		wanted := actionFile{Body: snapshot.Body, Mode: fs.FileMode(snapshot.Mode), Exists: snapshot.Exists}
		if !actionPath(snapshot.Path) || total > MaxActionBytes || snapshot.BeforeHash != actionHash(wanted) {
			return errors.New("invalid owned action snapshot")
		}
		actual, err := actionRead(root, snapshot.Path)
		if err != nil {
			return err
		}
		if !actionEqual(actual, wanted) {
			return ErrActionChanged
		}
	}
	return ctx.Err()
}
