package extension

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filememo"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type LinkedActionFile struct {
	Name        string
	Path        string
	BeforeWrite func(string) error
	Private     bool
	UnlinkLast  bool
	UnlinkName  bool
}

type linkedActionSnapshot struct {
	file   LinkedActionFile
	target string
	info   fs.FileInfo
	name   fs.FileInfo
	before actionFile
}

func readLinkedActionFile(file LinkedActionFile) (linkedActionSnapshot, error) {
	snapshot, err := inspectLinkedActionFile(file)
	if err == nil && file.UnlinkName {
		snapshot.name, err = os.Lstat(file.Path)
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}
	if err == nil && snapshot.before.Exists && snapshot.info.Mode().IsRegular() {
		snapshot.before.Body, err = os.ReadFile(snapshot.target)
	}
	return snapshot, err
}

func (snapshot linkedActionSnapshot) checkName() error {
	current, err := os.Lstat(snapshot.file.Path)
	if errors.Is(err, fs.ErrNotExist) && snapshot.name == nil ||
		err == nil && snapshot.name != nil && current.Mode() == snapshot.name.Mode() && os.SameFile(current, snapshot.name) {
		return nil
	}
	conflict := fmt.Errorf("%s: %w", snapshot.file.Path, ErrActionChanged)
	return edit.PreserveOnRollback(snapshot.file.Path, errors.Join(conflict, err))
}

func inspectLinkedActionFile(file LinkedActionFile) (linkedActionSnapshot, error) {
	snapshot := linkedActionSnapshot{file: file}
	path := file.Path
	stat := os.Stat
	if file.UnlinkLast {
		path = filepath.Dir(path)
		stat = os.Lstat
	}
	target, err := edit.Target(path)
	if err != nil {
		return snapshot, err
	}
	if file.UnlinkLast {
		target = filepath.Join(target, filepath.Base(file.Path))
	}
	snapshot.target = target
	info, err := stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		snapshot.target, err = missingLinkedTarget(target)
		return snapshot, err
	}
	if err != nil {
		return snapshot, err
	}
	snapshot.info = info
	snapshot.before = actionFile{Exists: true, Mode: info.Mode().Perm()}
	if file.UnlinkLast && info.Mode()&fs.ModeSymlink != 0 {
		name, err := os.Readlink(target)
		snapshot.before.Body = []byte(name)
		return snapshot, err
	}
	if !info.Mode().IsRegular() {
		return snapshot, fmt.Errorf("%s is not a regular settings file", file.Path)
	}
	return snapshot, nil
}

func missingLinkedTarget(path string) (string, error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		return path, nil
	}
	parent, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) && dir != path && os.IsPathSeparator(dir[len(dir)-1]) {
		dir = dir[:len(dir)-1]
		if _, statErr := os.Lstat(dir); errors.Is(statErr, fs.ErrNotExist) {
			parent, err = missingLinkedTarget(dir)
		}
	}
	if err != nil {
		return "", err
	}
	if base == "." || base == ".." {
		return "", fmt.Errorf("can't resolve missing linked settings path %q", path)
	}
	return filepath.Join(parent, base), nil
}

func (snapshot linkedActionSnapshot) check() error {
	same, err := snapshot.matchesFile()
	if err != nil {
		return edit.PreserveOnRollback(snapshot.file.Path, errors.Join(ErrActionChanged, err))
	}
	if !same {
		return edit.PreserveOnRollback(snapshot.file.Path, fmt.Errorf("%s: %w", snapshot.file.Path, ErrActionChanged))
	}
	return nil
}

func (snapshot linkedActionSnapshot) matchesFile() (bool, error) {
	current, err := inspectLinkedActionFile(snapshot.file)
	if err != nil {
		return false, err
	}
	if current.target != snapshot.target || current.before.Exists != snapshot.before.Exists ||
		current.before.Mode != snapshot.before.Mode {
		return false, nil
	}
	if current.info == nil {
		return true, nil
	}
	if !os.SameFile(current.info, snapshot.info) {
		return false, nil
	}
	if current.info.Mode()&fs.ModeSymlink != 0 {
		return bytes.Equal(current.before.Body, snapshot.before.Body), nil
	}
	stream, err := os.Open(current.target)
	if err != nil {
		return false, err
	}
	defer stream.Close()
	info, err := stream.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != snapshot.before.Mode || !os.SameFile(info, snapshot.info) {
		return false, nil
	}
	var buffer [16 * 1024]byte
	for remaining := snapshot.before.Body; len(remaining) > 0; {
		block := buffer[:min(len(buffer), len(remaining))]
		if _, err := io.ReadFull(stream, block); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return false, nil
			}
			return false, err
		}
		if !bytes.Equal(block, remaining[:len(block)]) {
			return false, nil
		}
		remaining = remaining[len(block):]
	}
	count, err := stream.Read(buffer[:1])
	if err != nil && err != io.EOF {
		return false, err
	}
	return count == 0 && err == io.EOF, nil
}

func ApplyLinkedAgentPlan[Options any](options Options, planner func(Options, sdk.ActionSettings) (sdk.AgentPlan, error), files ...LinkedActionFile) (err error) {
	snapshots := make(map[string]linkedActionSnapshot, len(files))
	var originals []linkedActionSnapshot
	check := func() error {
		var changed error
		for _, file := range files {
			if snapshot, ok := snapshots[file.Name]; ok {
				changed = errors.Join(changed, snapshot.check())
			}
		}
		return changed
	}
	defer func() {
		if err == nil {
			return
		}
		err = errors.Join(err, check())
		for _, original := range slices.Backward(originals) {
			current := snapshots[original.file.Name]
			var restoreErr error
			if original.before.Exists {
				restoreErr = edit.RestoreAtomicChecked(current.target, original.before.Body, current.check, original.info)
			} else if restoreErr = current.check(); restoreErr == nil {
				restoreErr = os.Remove(current.target)
			}
			if restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore linked settings %s: %w", original.file.Path, restoreErr))
			}
		}
		for _, snapshot := range snapshots {
			err = edit.PreserveOnRollback(snapshot.file.Path, err)
		}
	}()
	settings := sdk.ActionSettings{Files: make([]sdk.ActionSettingFile, 0, len(files))}
	for _, file := range files {
		if _, exists := snapshots[file.Name]; exists {
			return fmt.Errorf("duplicate linked settings name %q", file.Name)
		}
		snapshot, err := readLinkedActionFile(file)
		if err != nil {
			return edit.PreserveOnRollback(file.Path, err)
		}
		for _, other := range snapshots {
			if snapshot.target == other.target || snapshot.info != nil && other.info != nil && os.SameFile(snapshot.info, other.info) {
				return fmt.Errorf("linked settings paths %q and %q name the same file", file.Path, other.file.Path)
			}
		}
		snapshots[file.Name] = snapshot
		before := snapshot.before
		settings.Files = append(settings.Files, sdk.ActionSettingFile{
			Path: file.Name, Body: bytes.Clone(before.Body), Exists: before.Exists, Mode: uint32(before.Mode), BeforeHash: actionHash(before),
		})
	}
	plan, err := planner(options, settings)
	if err != nil {
		if failure, ok := err.(*edit.FileError); ok {
			if snapshot, exists := snapshots[failure.Path]; exists {
				return edit.AtFile(snapshot.file.Path, failure.Err)
			}
		}
		return err
	}
	if plan.Binding != nil {
		return errors.New("linked settings plan returned a binding change")
	}
	if len(plan.Files) == 0 {
		return check()
	}
	touched := make(map[string]bool, len(plan.Files))
	for index, change := range plan.Files {
		snapshot, ok := snapshots[change.Path]
		if !ok || touched[change.Path] || change.BeforeHash != actionHash(snapshot.before) || change.Remove && len(change.Body) != 0 || change.Mode != 0 {
			return fmt.Errorf("invalid linked settings change for %q", change.Path)
		}
		if snapshot.file.UnlinkLast && !change.Remove ||
			change.Remove && (snapshot.file.UnlinkLast || snapshot.file.UnlinkName) && index != len(plan.Files)-1 {
			return fmt.Errorf("linked settings unlink for %q must be the final change", change.Path)
		}
		touched[change.Path] = true
	}
	if err := check(); err != nil {
		return err
	}
	for _, change := range plan.Files {
		snapshot := snapshots[change.Path]
		if snapshot.file.BeforeWrite != nil {
			if err := snapshot.file.BeforeWrite(snapshot.file.Path); err != nil {
				return err
			}
		}
		if snapshot.file.UnlinkLast || snapshot.file.UnlinkName && change.Remove {
			if err := check(); err != nil {
				return err
			}
			target := snapshot.target
			if snapshot.file.UnlinkName {
				if err := snapshot.checkName(); err != nil {
					return err
				}
				target = snapshot.file.Path
			}
			if snapshot.before.Exists {
				if err := os.Remove(target); err != nil {
					return edit.AtFile(snapshot.file.Path, err)
				}
				filememo.Forget()
			}
			return nil
		}
		var written edit.AtomicPublication
		var unlinked bool
		if !change.Remove {
			if !snapshot.before.Exists && snapshot.file.Path != snapshot.target {
				if err := appdir.Gone(filepath.Dir(snapshot.file.Path)); err != nil {
					return err
				}
			}
			write := edit.WriteAtomicChecked
			if snapshot.file.Private {
				write = edit.WritePrivateAtomicChecked
			}
			err = write(snapshot.target, change.Body, check, &written)
		} else if snapshot.before.Exists {
			unlinked, err = edit.RemoveChecked(snapshot.file.Path, check, &written)
		} else {
			err = check()
		}
		if unlinked {
			originals = append(originals, snapshot)
			snapshot.info = nil
			snapshot.before = actionFile{}
			snapshots[change.Path] = snapshot
		} else if written.Info != nil {
			originals = append(originals, snapshot)
			body := written.Body
			if written.RetainedTail && len(body) < len(snapshot.before.Body) {
				body = append(bytes.Clone(body), snapshot.before.Body[len(body):]...)
			}
			snapshot.info = written.Info
			snapshot.before = actionFile{Exists: true, Mode: written.Info.Mode().Perm(), Body: body}
			snapshots[change.Path] = snapshot
		}
		if err != nil {
			return err
		}
	}
	return nil
}
