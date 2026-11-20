package steady

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"testing"
)

func TestCheckedRetryProtectsAnEditDuringTheWindowsDelay(t *testing.T) {
	previousWindows := windows
	windows = true
	t.Cleanup(func() { windows = previousWindows })
	stale := errors.New("replacement is stale")
	attempts, checks := 0, 0
	err := retryChecked(func() error {
		checks++
		if attempts != 0 {
			return stale
		}
		return nil
	}, func() error {
		attempts++
		if attempts == 1 {
			return fs.ErrPermission
		}
		return nil
	})
	if !errors.Is(err, stale) || attempts != 1 || checks != 2 {
		t.Fatalf("stale replacement attempted: error=%v attempts=%d checks=%d", err, attempts, checks)
	}
}

// as Windows has it: a file being renamed over is, for a moment, not there
func TestStatWaitsOutAReplace(t *testing.T) {
	windows = true
	defer func() { windows, stat, readFile = runtime.GOOS == "windows", os.Stat, os.ReadFile }()
	missing := 3
	stat = func(p string) (fs.FileInfo, error) {
		if missing > 0 {
			missing--
			return nil, fs.ErrNotExist
		}
		return os.Stat(p)
	}
	p := t.TempDir()
	if _, err := Stat(p); err != nil {
		t.Fatalf("a file being replaced taken for gone: %v", err)
	}
	missing = 3
	readFile = func(p string) ([]byte, error) {
		if missing > 0 {
			missing--
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		}
		return []byte("accounts"), nil
	}
	if b, err := ReadThere(p); err != nil || string(b) != "accounts" {
		t.Fatalf("read %q, %v", b, err)
	}
	// one really gone is said so, after the wait
	stat = os.Stat
	if _, err := Stat(p + "-none"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a file not there: %v", err)
	}
}

// #1665: a file whose folder isn't there is not being renamed into it, and
// is said gone without the wait; most agents aren't installed, and each
// one's first look waited the whole of it
func TestStatOfAFileWithNoFolderWaitsNothing(t *testing.T) {
	windows = true
	defer func() { windows, stat = runtime.GOOS == "windows", os.Stat }()
	looks := 0
	stat = func(p string) (fs.FileInfo, error) {
		looks++
		return nil, &fs.PathError{Op: "stat", Path: p, Err: errors.Join(fs.ErrNotExist, errPathNotFound)}
	}
	if _, err := Stat("no-folder/settings.json"); !errors.Is(err, fs.ErrNotExist) || looks != 1 {
		t.Fatalf("looked %d times: %v", looks, err)
	}
	// its folder there, the file not, is waited on as before
	looks = 0
	stat = func(p string) (fs.FileInfo, error) {
		looks++
		return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	if _, err := Stat("settings.json"); !errors.Is(err, fs.ErrNotExist) || looks < 2 {
		t.Fatalf("looked %d times: %v", looks, err)
	}
}
