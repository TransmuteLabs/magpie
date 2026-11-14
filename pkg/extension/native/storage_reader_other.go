//go:build !linux && !darwin

package native

import (
	"context"
	"errors"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func openStorageAccess(context.Context, sdk.StorageNativeAccess) (*StorageRoot, error) {
	return nil, errors.New("rooted native storage descriptors are not supported on this platform")
}

func (*StorageRoot) openFile(string) (*os.File, error) {
	return nil, errors.New("rooted native storage is not supported on this platform")
}
