package extension

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

func (grant *StorageGrant) open(name string) (*os.File, error) {
	if !grant.family.Allows(name) {
		return nil, os.ErrPermission
	}
	parent, err := syscall.Dup(int(grant.directory.Fd()))
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
	file := os.NewFile(uintptr(parent), "storage-family-file")
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() && !info.IsDir() {
		file.Close()
		return nil, errors.New("unsupported storage file")
	}
	return file, nil
}
