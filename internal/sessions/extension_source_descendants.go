package sessions

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type sessionSourceDirectory struct {
	path    string
	info    os.FileInfo
	entries []string
}

type sessionSourceTreeSnapshot struct {
	journal     string
	rule        sdk.SessionSourceDescendants
	directories []sessionSourceDirectory
}

func sourceDescendantIDs(identities []sdk.SessionSourceIdentity) []string {
	var ids []string
	for _, identity := range identities {
		ids = append(ids, identity.SessionID)
	}
	return ids
}

func refreshSourceDescendantSnapshots(ctx context.Context) {
	for ctx.Err() == nil {
		view := sourceView.Load()
		if view == nil {
			return
		}
		records := slices.Clone(view.records)
		changed := false
		for index, record := range records {
			if record.stale || record.snapshot == nil || record.snapshot.descendants == nil || record.snapshot.check(ctx) == nil {
				continue
			}
			stale := *record
			stale.stale, stale.summary = true, record.summary.clone()
			records[index] = &stale
			changed = true
		}
		if !changed || ctx.Err() != nil {
			return
		}
		next := *view
		next.records = records
		if sourceView.CompareAndSwap(view, &next) {
			return
		}
	}
}

func collectSourceDescendants(ctx context.Context, grant sourceGrant, request *sdk.SessionSourceReadRequest, snapshot *sessionInputSnapshot) error {
	if grant.descriptor.Descendants == nil {
		return nil
	}
	if err := checkSessionSourceAdmission(ctx); err != nil {
		return err
	}
	root, err := os.OpenRoot(grant.root)
	if err != nil {
		return err
	}
	defer root.Close()
	tree, names, err := scanSourceDescendants(ctx, root, request.Locator.Path, *grant.descriptor.Descendants)
	if err != nil {
		return err
	}
	total := 0
	seen := map[string]bool{}
	for _, file := range snapshot.files {
		total += int(file.info.Size())
		seen[sessionInputKey(file.path)] = true
	}
	for _, name := range names {
		if seen[sessionInputKey(name)] || len(snapshot.files) >= extension.MaxActionFiles {
			return errors.New("duplicate session source descendant or document budget exceeded")
		}
		file, body, err := readSessionInput(ctx, root, name, extension.MaxActionBytes-total)
		if err != nil {
			return err
		}
		total += len(body)
		seen[sessionInputKey(name)] = true
		snapshot.files = append(snapshot.files, file)
		complete := bytes.LastIndexByte(body, '\n') + 1
		request.Documents = append(request.Documents, sdk.SessionDocument{
			Path: name, Format: grant.descriptor.Codec, Body: bytes.Clone(body[:complete]),
		})
		request.Descendants = append(request.Descendants, name)
	}
	snapshot.descendants = tree
	if err := snapshot.check(ctx); err != nil {
		return err
	}
	return checkSessionSourceAdmission(ctx)
}

func scanSourceDescendants(ctx context.Context, root *os.Root, journal string, rule sdk.SessionSourceDescendants) (*sessionSourceTreeSnapshot, []string, error) {
	tree := &sessionSourceTreeSnapshot{journal: journal, rule: rule}
	tree.rule.ExcludeDirectories = slices.Clone(rule.ExcludeDirectories)
	base := strings.TrimSuffix(journal, ".jsonl")
	if !sdk.SessionSourceDescendantPath(&rule, journal, base+"/probe.jsonl") {
		return nil, nil, errors.New("invalid source descendant tree")
	}
	var names []string
	count := 0
	var scan func(string, int) error
	scan = func(relative string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > 16 || !sdk.SessionSourcePath(relative) {
			return errors.New("session source descendant depth or path budget exceeded")
		}
		before, err := root.Lstat(relative)
		if depth == 0 && errors.Is(err, fs.ErrNotExist) {
			tree.directories = append(tree.directories, sessionSourceDirectory{path: relative})
			return nil
		}
		if err != nil {
			return err
		}
		if !before.IsDir() {
			return errors.New("session source descendants require non-symlink directories")
		}
		directory, err := root.Open(relative)
		if err != nil {
			return err
		}
		defer directory.Close()
		opened, err := directory.Stat()
		if err != nil || !opened.IsDir() || !os.SameFile(before, opened) || !sessionStoreModeEqual(before.Mode(), opened.Mode()) {
			return extension.ErrActionChanged
		}
		current := sessionSourceDirectory{path: relative, info: opened}
		var subdirectories []string
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries, err := directory.ReadDir(128)
			for _, entry := range entries {
				count++
				if count > 10000 {
					return errors.New("session source descendant enumeration budget exceeded")
				}
				if entry.IsDir() && slices.Contains(rule.ExcludeDirectories, entry.Name()) {
					continue
				}
				name := path.Join(relative, entry.Name())
				if entry.Type()&fs.ModeSymlink != 0 {
					return errors.New("session source descendant symlinks are not admitted")
				}
				switch {
				case entry.IsDir():
					current.entries = append(current.entries, entry.Name()+"/")
					subdirectories = append(subdirectories, name)
				case strings.HasSuffix(entry.Name(), ".jsonl"):
					if !sdk.SessionSourceDescendantPath(&rule, journal, name) || !entry.Type().IsRegular() {
						return errors.New("invalid session source descendant journal")
					}
					current.entries = append(current.entries, entry.Name())
					names = append(names, name)
					if len(names) >= extension.MaxActionFiles {
						return errors.New("session source descendant document budget exceeded")
					}
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
		tree.directories = append(tree.directories, current)
		slices.Sort(subdirectories)
		for _, name := range subdirectories {
			if err := scan(name, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := scan(base, 0); err != nil {
		return nil, nil, err
	}
	slices.Sort(names)
	return tree, names, nil
}

func (tree *sessionSourceTreeSnapshot) check(ctx context.Context, root *os.Root) error {
	current, _, err := scanSourceDescendants(ctx, root, tree.journal, tree.rule)
	if err != nil {
		return err
	}
	if len(current.directories) != len(tree.directories) {
		return extension.ErrActionChanged
	}
	for index, expected := range tree.directories {
		actual := current.directories[index]
		if expected.path != actual.path || !slices.Equal(expected.entries, actual.entries) ||
			(expected.info == nil) != (actual.info == nil) {
			return extension.ErrActionChanged
		}
		if expected.info != nil && (!os.SameFile(expected.info, actual.info) ||
			!sessionStoreModeEqual(expected.info.Mode(), actual.info.Mode())) {
			return extension.ErrActionChanged
		}
	}
	return nil
}
