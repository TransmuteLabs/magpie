//go:build !unix

package extension

import (
	"fmt"
	"os"
)

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
