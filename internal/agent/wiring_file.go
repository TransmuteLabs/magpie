package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/steady"
)

type wiringFile struct {
	target string
	info   fs.FileInfo
	body   []byte
}

func (file wiringFile) check(path string) error {
	after, err := readWiringFile(path)
	if err == nil && file.same(after) {
		return nil
	}
	conflict := fmt.Errorf("agent file %s: %w", path, extension.ErrActionChanged)
	return edit.PreserveOnRollback(path, errors.Join(conflict, err))
}

func (file wiringFile) preserveOnError(path string, err error) error {
	if err != nil {
		if changed := file.check(path); changed != nil {
			return errors.Join(err, changed)
		}
	}
	return err
}

func (file wiringFile) same(other wiringFile) bool {
	if file.target != other.target || !bytes.Equal(file.body, other.body) {
		return false
	}
	if file.info == nil || other.info == nil {
		return file.info == nil && other.info == nil
	}
	return file.info.Mode() == other.info.Mode() && os.SameFile(file.info, other.info)
}

func readWiringFile(path string) (wiringFile, error) {
	target, err := edit.Target(path)
	if err != nil {
		return wiringFile{}, err
	}
	info, err := os.Stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return wiringFile{target: target}, nil
	}
	if err != nil {
		return wiringFile{}, err
	}
	if !info.Mode().IsRegular() {
		return wiringFile{}, fmt.Errorf("agent file %s is not a regular file", path)
	}
	body, err := steady.ReadFile(target)
	return wiringFile{target: target, info: info, body: body}, err
}
