package provider

import (
	"os"
	"path/filepath"
	"strings"
)

func stagedWriteFor(file *os.File, target string) bool {
	suffix, ok := strings.CutPrefix(filepath.Base(file.Name()), "."+filepath.Base(target)+".")
	suffix = strings.TrimSuffix(suffix, ".tmp")
	if !ok || suffix == "" || strings.Trim(suffix, "0123456789") != "" {
		return false
	}
	stagingDir, targetDir := filepath.Dir(file.Name()), filepath.Dir(target)
	if stagingDir == targetDir {
		return true
	}
	staged, err := os.Stat(stagingDir)
	if err != nil {
		return false
	}
	destination, err := os.Stat(targetDir)
	return err == nil && os.SameFile(staged, destination)
}
