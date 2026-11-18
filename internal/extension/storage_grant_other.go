//go:build !linux && !darwin

package extension

import (
	"errors"
	"io/fs"
	"os"
)

func (grant *StorageGrant) open(string) (*os.File, error) {
	return nil, errors.New("rooted native storage is not supported on this platform")
}

func (*StorageGrant) stat(string) (fs.FileInfo, error) {
	return nil, errors.New("rooted native storage is not supported on this platform")
}

func storageSameFile(first, second fs.FileInfo) bool {
	return first != nil && second != nil && os.SameFile(first, second)
}
