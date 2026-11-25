package extension

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
	"golang.org/x/sys/unix"
)

func TestAccountStampMetadataDoesNotOpenTrackedFilesOnLinux(t *testing.T) {
	root := t.TempDir()
	catalogPath := filepath.Join(root, "catalog.json")
	if err := os.WriteFile(catalogPath, []byte(`{"version":1,"modules":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	scope := sdk.Scope{Plugin: "fixture.stamp"}
	if _, err := store.SaveAccount(Account{
		Plugin:   scope.Plugin,
		Provider: "vendor",
		ID:       "alice",
		Auth:     json.RawMessage(`{"token":"synthetic"}`),
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StateCAS(scope, moduleConfigKey, 0, json.RawMessage(`{"enabled":true}`)); err != nil {
		t.Fatal(err)
	}
	accountsPath := filepath.Join(root, "private", scope.Plugin, "accounts.json")
	configPath := filepath.Join(root, "private", scope.Plugin, "state", storeStateName(scope, moduleConfigKey))

	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	for _, watched := range []string{catalogPath, accountsPath, configPath} {
		if _, err := unix.InotifyAddWatch(fd, watched, unix.IN_OPEN|unix.IN_CLOSE_NOWRITE); err != nil {
			t.Fatal(err)
		}
	}

	before, err := accountStampAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if opens := drainInotifyOpens(t, fd); opens != 0 {
		t.Fatalf("accountStampAt opened tracked files %d times; want 0", opens)
	}

	original, err := os.Lstat(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "catalog.replacement")
	if err := os.WriteFile(replacement, []byte(`{"version":2,"modules":[]}`), original.Mode()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, time.Now(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, catalogPath); err != nil {
		t.Fatal(err)
	}
	current, err := os.Lstat(catalogPath)
	if err != nil || os.SameFile(original, current) || current.Size() != original.Size() ||
		current.Mode() != original.Mode() || !current.ModTime().Equal(original.ModTime()) {
		t.Fatalf("fixture did not replace catalog.json with identical metadata: %v", err)
	}

	if _, err := unix.InotifyAddWatch(fd, catalogPath, unix.IN_OPEN|unix.IN_CLOSE_NOWRITE); err != nil {
		t.Fatal(err)
	}
	drainInotifyOpens(t, fd)
	after, err := accountStampAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if after.Equal(before) {
		t.Fatal("atomic catalog replacement with preserved metadata retained AccountFileStamp")
	}
	if opens := drainInotifyOpens(t, fd); opens != 0 {
		t.Fatalf("post-replacement accountStampAt opened tracked files %d times; want 0", opens)
	}
}

func drainInotifyOpens(t *testing.T, fd int) int {
	t.Helper()
	opens := 0
	var events [4096]byte
	for {
		size, err := unix.Read(fd, events[:])
		if err == unix.EAGAIN {
			return opens
		}
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if size == 0 || size%unix.SizeofInotifyEvent != 0 {
			t.Fatalf("unexpected inotify event bytes: %d", size)
		}
		for offset := 0; offset < size; offset += unix.SizeofInotifyEvent {
			mask := binary.NativeEndian.Uint32(events[offset+4:])
			if mask&(unix.IN_OPEN|unix.IN_CLOSE_NOWRITE) != 0 {
				opens++
			}
		}
	}
}
