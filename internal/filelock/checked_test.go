package filelock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckedMutexRequiresFileLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.lock")
	mutex := Mutex{Path: func() string { return path }}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if mutex.LockChecked() {
		mutex.Unlock()
		t.Fatal("checked acquisition accepted an unavailable file lock")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !mutex.LockChecked() {
		t.Fatal("failed checked acquisition retained the local mutex")
	}
	unlock, held := Lock(path, time.Millisecond)
	unlock()
	if held {
		mutex.Unlock()
		t.Fatal("checked acquisition did not hold the file lock")
	}
	mutex.Unlock()
	unlock, held = Lock(path, time.Second)
	unlock()
	if !held {
		t.Fatal("checked acquisition did not release the file lock")
	}
}

func TestLegacyMutexKeepsUnavailableLockFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.lock")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	called := 0
	mutex := Mutex{
		Path:   func() string { return path },
		Locked: func() { called++ },
	}
	mutex.Lock()
	mutex.Unlock()
	if called != 1 {
		t.Fatal("legacy lock fallback stopped refreshing the guarded snapshot")
	}
	if mutex.LockChecked() {
		mutex.Unlock()
		t.Fatal("checked acquisition accepted the legacy fallback")
	}
}
