package memembed

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/yetone/magpie/internal/filelock"
)

var errIndexWithdrawn = errors.New("Memory semantic indexing stopped")

type indexRun struct {
	generation indexGeneration
	vectors    *vecs
	unlock     func()
}

type indexGeneration struct {
	switchFile fs.FileInfo
	modelDir   fs.FileInfo
}

func readIndexGeneration() (indexGeneration, error) {
	switchFile, err := os.Stat(StatePath())
	if err != nil {
		return indexGeneration{}, err
	}
	modelDir, err := os.Stat(dir())
	if err != nil {
		return indexGeneration{}, err
	}
	return indexGeneration{switchFile: switchFile, modelDir: modelDir}, nil
}

func (generation indexGeneration) same(other indexGeneration) bool {
	return os.SameFile(generation.switchFile, other.switchFile) &&
		generation.switchFile.ModTime().Equal(other.switchFile.ModTime()) &&
		os.SameFile(generation.modelDir, other.modelDir)
}

func openIndex() (*indexRun, error) {
	if !On() || !Installed() {
		return nil, nil
	}
	if !stateMu.LockChecked() {
		return nil, errStateLock
	}
	defer stateMu.Unlock()
	if !On() || !Installed() {
		return nil, nil
	}
	generation, err := readIndexGeneration()
	if err != nil {
		return nil, err
	}
	unlock, held := filelock.Lock(filepath.Join(dir(), "vectors.lock"), 0)
	if !held {
		return nil, nil
	}
	return &indexRun{generation: generation, vectors: vectorsAt(generation), unlock: unlock}, nil
}

func (run *indexRun) current() bool {
	generation, err := readIndexGeneration()
	return err == nil && run.generation.same(generation) && On()
}

func (run *indexRun) publish() error {
	if !stateMu.LockChecked() {
		return errStateLock
	}
	defer stateMu.Unlock()
	if !run.current() {
		return errIndexWithdrawn
	}
	return run.vectors.save()
}
