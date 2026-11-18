//go:build linux || darwin

package native

import (
	"context"
	"os"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
	"golang.org/x/sys/unix"
)

func openStorageAccess(ctx context.Context, access sdk.StorageNativeAccess) (*StorageRoot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	descriptor, err := unix.FcntlInt(uintptr(access.Descriptor), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	keep := os.NewFile(uintptr(descriptor), "native-storage-root")
	info, err := keep.Stat()
	if err != nil || !info.IsDir() {
		keep.Close()
		return nil, os.ErrPermission
	}
	return &StorageRoot{keep: keep, ctx: ctx, family: access.Family, lease: access.Lease}, nil
}

func (root *StorageRoot) openFile(name string) (*os.File, error) {
	parent, err := unix.FcntlInt(root.keep.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(name, "/")
	for index, part := range parts {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if index < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, openErr := unix.Openat(parent, part, flags, 0)
		unix.Close(parent)
		if openErr != nil {
			return nil, openErr
		}
		parent = next
	}
	return os.NewFile(uintptr(parent), "native-storage-file"), nil
}
