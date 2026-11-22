package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// TryLock takes f's lock when no other process (or other handle of the
// file) holds it.
func TryLock(f *os.File) (bool, error) {
	return tryLock(f, windows.LOCKFILE_EXCLUSIVE_LOCK)
}

func TryShared(f *os.File) (bool, error) {
	return tryLock(f, 0)
}

func tryLock(file *os.File, mode uint32) (bool, error) {
	err := windows.LockFileEx(windows.Handle(file.Fd()), mode|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

// Unlock lets f's lock go.
func Unlock(f *os.File) {
	windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}
