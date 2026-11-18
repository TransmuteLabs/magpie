//go:build linux || darwin

package sessions

import (
	"io/fs"
	"os"
)

func sourceRootFileIdentity(_ *os.Root, info fs.FileInfo) (string, error) {
	return storageRootFileIdentity(info)
}
