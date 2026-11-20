package extension

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
)

// PublishSessionDocument stages a linked converter's document in the selected
// root. The host supplies its checks on the input and target store. A late
// failure rolls back only the unchanged file this operation created.
func PublishSessionDocument(root *os.Root, name string, write func(io.Writer) error, check func(created map[string]fs.FileInfo, published bool) error) (fs.FileInfo, error) {
	if !actionPath(name) {
		return nil, errors.New("invalid session document path")
	}
	if err := check(nil, false); err != nil {
		return nil, err
	}
	directories, err := actionParents(root, name)
	if err != nil {
		return nil, err
	}
	created := make(map[string]fs.FileInfo, len(directories))
	for _, directory := range directories {
		info, err := root.Lstat(directory)
		if err != nil || !info.IsDir() {
			return nil, errors.Join(ErrActionChanged, err)
		}
		created[directory] = info
	}
	parent, fileName := path.Dir(name), path.Base(name)
	parentInfo, err := actionInfo(root, parent)
	if err != nil || !parentInfo.IsDir() {
		return nil, errors.Join(ErrActionChanged, err)
	}
	directory, err := root.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	opened, err := directory.Stat(".")
	if err != nil || !os.SameFile(parentInfo, opened) {
		return nil, errors.Join(ErrActionChanged, err)
	}
	revalidate := func(published bool) error {
		if err := check(created, published); err != nil {
			return err
		}
		info, err := actionInfo(root, parent)
		if err != nil || !info.IsDir() || !os.SameFile(parentInfo, info) {
			return errors.Join(ErrActionChanged, err)
		}
		return nil
	}
	digest := sha256.New()
	staged, info, err := actionStageWriter(directory, fileName, 0o600, func(file io.Writer) error {
		writer := bufio.NewWriter(io.MultiWriter(file, digest))
		if err := write(writer); err != nil {
			return err
		}
		return writer.Flush()
	})
	if err != nil {
		return nil, err
	}
	defer actionRemoveOwned(directory, staged, info)
	if err := revalidate(false); err != nil {
		return nil, err
	}
	if err := directory.Link(staged, fileName); err != nil {
		if errors.Is(err, fs.ErrExist) {
			err = errors.Join(ErrActionChanged, err)
		}
		return nil, err
	}
	if err := actionSync(directory, "."); err != nil {
		return nil, errors.Join(err, rollbackSessionDocument(directory, fileName, info, digest.Sum(nil)))
	}
	if err := revalidate(true); err != nil {
		return nil, errors.Join(err, rollbackSessionDocument(directory, fileName, info, digest.Sum(nil)))
	}
	current, err := directory.Lstat(fileName)
	if err != nil || !SameSessionDocument(info, current) {
		return nil, errors.Join(ErrActionChanged, err)
	}
	return info, nil
}

// SameSessionDocument checks the file identity and metadata of a publication.
func SameSessionDocument(expected, current fs.FileInfo) bool {
	return expected != nil && current != nil && current.Mode().IsRegular() &&
		os.SameFile(expected, current) && expected.Mode() == current.Mode() &&
		expected.Size() == current.Size() && expected.ModTime().Equal(current.ModTime())
}

func rollbackSessionDocument(root *os.Root, name string, info fs.FileInfo, digest []byte) error {
	before, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !SameSessionDocument(info, before) {
		return errors.Join(ErrActionChanged, err)
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil || !SameSessionDocument(info, opened) {
		return errors.Join(ErrActionChanged, err, file.Close())
	}
	hash := sha256.New()
	_, err = io.Copy(hash, io.LimitReader(file, info.Size()+1))
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	after, err := root.Lstat(name)
	if err != nil || !SameSessionDocument(info, after) || !bytes.Equal(hash.Sum(nil), digest) {
		return errors.Join(ErrActionChanged, err)
	}
	return root.Remove(name)
}
