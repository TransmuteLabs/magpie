//go:build !linux

package extension

import (
	"errors"
	"os"
)

func (grant *StorageGrant) open(string) (*os.File, error) {
	return nil, errors.New("rooted native storage is not supported on this platform")
}
