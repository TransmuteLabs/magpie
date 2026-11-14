package native

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func openStorageAccess(ctx context.Context, access sdk.StorageNativeAccess) (*StorageRoot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	descriptor, err := syscall.Dup(access.Descriptor)
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(descriptor)
	keep := os.NewFile(uintptr(descriptor), "native-storage-root")
	root, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", descriptor))
	if err != nil {
		keep.Close()
		return nil, err
	}
	return &StorageRoot{root: root, keep: keep, ctx: ctx, family: access.Family, lease: access.Lease}, nil
}

func (root *StorageRoot) openFile(name string) (*os.File, error) {
	parent, err := syscall.Dup(int(root.keep.Fd()))
	if err != nil {
		return nil, err
	}
	parts := strings.Split(name, "/")
	for index, part := range parts {
		flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
		if index < len(parts)-1 {
			flags |= syscall.O_DIRECTORY
		}
		next, openErr := syscall.Openat(parent, part, flags, 0)
		syscall.Close(parent)
		if openErr != nil {
			return nil, openErr
		}
		parent = next
	}
	return os.NewFile(uintptr(parent), "native-storage-file"), nil
}
