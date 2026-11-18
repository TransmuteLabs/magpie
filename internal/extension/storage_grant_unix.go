//go:build linux || darwin

package extension

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func (grant *StorageGrant) open(name string) (*os.File, error) {
	if !grant.family.Allows(name) {
		return nil, os.ErrPermission
	}
	if grant.lifetime != nil && grant.lifetime.native && len(grant.lifetime.deferred) >= storageMaxHeldDescriptors {
		return nil, errors.New("native storage descriptor budget exhausted")
	}
	parent, err := unix.FcntlInt(grant.directory.Fd(), unix.F_DUPFD_CLOEXEC, 0)
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
	file := os.NewFile(uintptr(parent), "storage-family-file")
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() && !info.IsDir() {
		grant.closeMember(file)
		return nil, errors.New("unsupported storage file")
	}
	return file, nil
}

type storageUnixInfo struct {
	name string
	stat unix.Stat_t
}

func (info storageUnixInfo) Name() string { return path.Base(info.name) }
func (info storageUnixInfo) Size() int64  { return info.stat.Size }
func (info storageUnixInfo) Mode() fs.FileMode {
	mode := fs.FileMode(info.stat.Mode & 0o777)
	if info.IsDir() {
		mode |= fs.ModeDir
	}
	if info.stat.Mode&unix.S_ISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if info.stat.Mode&unix.S_ISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if info.stat.Mode&unix.S_ISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}
func (info storageUnixInfo) ModTime() time.Time {
	return time.Unix(info.stat.Mtim.Sec, info.stat.Mtim.Nsec)
}
func (info storageUnixInfo) IsDir() bool { return info.stat.Mode&unix.S_IFMT == unix.S_IFDIR }
func (info storageUnixInfo) Sys() any {
	return &syscall.Stat_t{Dev: info.stat.Dev, Ino: info.stat.Ino, Mode: info.stat.Mode}
}

func storageSameFile(first, second fs.FileInfo) bool {
	if first == nil || second == nil {
		return false
	}
	firstStat, firstOK := first.Sys().(*syscall.Stat_t)
	secondStat, secondOK := second.Sys().(*syscall.Stat_t)
	return firstOK && secondOK && firstStat.Dev == secondStat.Dev && firstStat.Ino == secondStat.Ino
}

func (grant *StorageGrant) stat(name string) (fs.FileInfo, error) {
	if !grant.family.Allows(name) {
		return nil, fs.ErrPermission
	}
	parent, err := unix.FcntlInt(grant.directory.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(name, "/")
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(parent, part, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY, 0)
		unix.Close(parent)
		if openErr != nil {
			return nil, openErr
		}
		parent = next
	}
	defer unix.Close(parent)
	info := storageUnixInfo{name: name}
	if err := unix.Fstatat(parent, parts[len(parts)-1], &info.stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	if kind := info.stat.Mode & unix.S_IFMT; kind != unix.S_IFREG && kind != unix.S_IFDIR {
		return nil, fs.ErrPermission
	}
	return info, nil
}
