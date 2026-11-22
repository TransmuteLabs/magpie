package extension

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type AccountFileStamp struct {
	sum   [32]byte
	files []os.FileInfo
}

func (stamp AccountFileStamp) Equal(other AccountFileStamp) bool {
	if stamp.sum != other.sum || len(stamp.files) != len(other.files) {
		return false
	}
	for index, info := range stamp.files {
		if !os.SameFile(info, other.files[index]) {
			return false
		}
	}
	return true
}

func AccountStamp() (AccountFileStamp, error) {
	return accountStampAt(filepath.Dir(CatalogPath()))
}

func accountStampAt(root string) (AccountFileStamp, error) {
	empty := AccountFileStamp{sum: sha256.Sum256(nil)}
	path := filepath.Join(root, "private")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return AccountFileStamp{}, fmt.Errorf("extension account stamp directory is unavailable")
	}
	directory, err := os.OpenRoot(path)
	if err != nil {
		return AccountFileStamp{}, err
	}
	defer directory.Close()
	opened, err := directory.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return AccountFileStamp{}, fmt.Errorf("extension account stamp directory changed")
	}
	list, err := directory.Open(".")
	if err != nil {
		return AccountFileStamp{}, err
	}
	entries, readErr := list.ReadDir(-1)
	closeErr := list.Close()
	if readErr != nil || closeErr != nil {
		return AccountFileStamp{}, fmt.Errorf("extension account stamp entries are unavailable")
	}
	slices.SortFunc(entries, func(left, right os.DirEntry) int {
		return strings.Compare(left.Name(), right.Name())
	})
	hash := sha256.New()
	var files []os.FileInfo
	for _, entry := range entries {
		if entry.Name() == ".lock" {
			continue
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return AccountFileStamp{}, fmt.Errorf("extension account namespace is unavailable")
		}
		namespace, err := directory.OpenRoot(entry.Name())
		if err != nil {
			return AccountFileStamp{}, err
		}
		type stampSource struct {
			name string
			info os.FileInfo
			err  error
		}
		var sources []stampSource
		if entry.Name() == savedSignInsDir {
			info, err := accountFileInfo(namespace, subscriptionOwnersFile)
			sources = append(sources, stampSource{subscriptionOwnersFile, info, err})
		} else {
			accountInfo, accountErr := accountFileInfo(namespace, "accounts.json")
			configInfo, configErr := accountConfigFileInfo(namespace, entry.Name())
			sources = append(sources, stampSource{"accounts.json", accountInfo, accountErr}, stampSource{moduleConfigKey, configInfo, configErr})
		}
		namespace.Close()
		for _, source := range sources {
			if errors.Is(source.err, os.ErrNotExist) {
				continue
			}
			if source.err != nil || !source.info.Mode().IsRegular() {
				return AccountFileStamp{}, fmt.Errorf("extension account/config stamp file is unavailable")
			}
			hash.Write([]byte(entry.Name()))
			hash.Write([]byte{0})
			hash.Write([]byte(source.name))
			hash.Write([]byte{0})
			files = append(files, source.info)
			var metadata [24]byte
			binary.LittleEndian.PutUint64(metadata[:8], uint64(source.info.ModTime().UnixNano()))
			binary.LittleEndian.PutUint64(metadata[8:16], uint64(source.info.Size()))
			binary.LittleEndian.PutUint64(metadata[16:], uint64(source.info.Mode()))
			hash.Write(metadata[:])
		}
	}
	result := AccountFileStamp{files: files}
	copy(result.sum[:], hash.Sum(nil))
	return result, nil
}

func accountConfigFileInfo(namespace *os.Root, plugin string) (os.FileInfo, error) {
	named, err := namespace.Lstat("state")
	if err != nil {
		return nil, err
	}
	if !named.IsDir() || named.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("extension configuration stamp directory is unavailable")
	}
	directory, err := namespace.OpenRoot("state")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	opened, err := directory.Stat(".")
	if err != nil || !os.SameFile(named, opened) {
		return nil, fmt.Errorf("extension configuration stamp directory changed")
	}
	return accountFileInfo(directory, storeStateName(sdk.Scope{Plugin: plugin}, moduleConfigKey))
}

func accountFileInfo(namespace *os.Root, name string) (os.FileInfo, error) {
	named, err := namespace.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !named.Mode().IsRegular() {
		return nil, fmt.Errorf("extension account stamp file is unavailable")
	}
	file, err := namespace.Open(name)
	if err != nil {
		return nil, err
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	current, namedErr := namespace.Lstat(name)
	if statErr != nil || closeErr != nil || namedErr != nil || !info.Mode().IsRegular() ||
		!current.Mode().IsRegular() || !os.SameFile(info, current) ||
		named.Size() != info.Size() || !named.ModTime().Equal(info.ModTime()) {
		return nil, fmt.Errorf("extension account stamp file changed")
	}
	return info, nil
}
