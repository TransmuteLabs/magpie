//go:build linux || darwin

package sessions

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
)

func storageRootFileIdentity(info fs.FileInfo) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return "", errors.New("storage root has no native directory identity")
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), nil
}
