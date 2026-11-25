//go:build unix

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
	return named, nil
}
