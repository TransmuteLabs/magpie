//go:build !linux && !darwin

package sessions

import (
	"errors"
	"io/fs"
)

func storageRootFileIdentity(fs.FileInfo) (string, error) {
	return "", errors.New("native own-storage sources are not supported on this platform")
}
