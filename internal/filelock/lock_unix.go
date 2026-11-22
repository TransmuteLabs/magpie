//go:build !windows

package filelock

import (
	"errors"
	"os"
	"syscall"
)

// TryLock takes f's lock when no other process (or other descriptor of
// the file) holds it.
func TryLock(f *os.File) (bool, error) {
	return tryLock(f, syscall.LOCK_EX)
}

func TryShared(f *os.File) (bool, error) {
	return tryLock(f, syscall.LOCK_SH)
}

func tryLock(file *os.File, mode int) (bool, error) {
	err := syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

// Unlock lets f's lock go.
func Unlock(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
