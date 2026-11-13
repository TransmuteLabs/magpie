//go:build darwin || linux || freebsd

package agent

import (
	"errors"
	"os"
	"syscall"
)

func extensionTryLock(file *os.File, shared bool) (bool, error) {
	mode := syscall.LOCK_EX
	if shared {
		mode = syscall.LOCK_SH
	}
	err := syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func extensionUnlock(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
