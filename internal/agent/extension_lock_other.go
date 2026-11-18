//go:build !darwin && !linux && !freebsd && !windows

package agent

import (
	"errors"
	"os"
)

func extensionTryLock(*os.File, bool) (bool, error) {
	return false, errors.New("agent profile ownership requires supported file locks")
}

func extensionUnlock(*os.File) {}
