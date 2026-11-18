//go:build !linux && !darwin && !windows

package sessions

import (
	"errors"
	"io/fs"
	"os"
)

func sourceRootFileIdentity(*os.Root, fs.FileInfo) (string, error) {
	return "", errors.New("session source directory identity is not supported on this platform")
}
