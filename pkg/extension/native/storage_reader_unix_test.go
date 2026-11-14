//go:build linux || darwin

package native

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
	"golang.org/x/sys/unix"
)

func TestStorageNativeRootIsFiniteReadonlyAndCancelable(t *testing.T) {
	path := t.TempDir()
	if err := os.WriteFile(filepath.Join(path, "history.db"), []byte("history"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("history.db", filepath.Join(path, "alias")); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	root, err := openStorageAccess(ctx, sdk.StorageNativeAccess{Descriptor: int(directory.Fd()),
		Family: sdk.StorageFamily{ID: "db", Primary: "history.db", Sidecars: []string{"alias"}},
		Lease:  sdk.StorageReadLease{Limits: sdk.StorageLimits{MaxReadBytes: 8}}})
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	directory.Close()
	file, err := root.Open("history.db")
	if err != nil {
		t.Fatal("root descriptor was not independently pinned", err)
	}
	if _, err := file.Write([]byte("overwrite")); err == nil {
		t.Fatal("native storage opened a writable descriptor")
	}
	file.Close()
	for _, name := range []string{"alias", "../history.db", "other", "/etc/passwd"} {
		if file, err := root.Open(name); err == nil {
			file.Close()
			t.Fatal("native storage escaped finite no-symlink family", name)
		}
	}
	if root.Consume(8) != nil || root.Consume(1) == nil {
		t.Fatal("native storage ignored aggregate byte budget")
	}
	cancel()
	if file, err := root.Open("history.db"); err == nil {
		file.Close()
		t.Fatal("canceled native root remained readable")
	}
}

func TestStorageNativeRootRetainsIdentityWithoutFollowingComponents(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "media"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "history.db"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(directory, "media", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(directory, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	borrowed, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer borrowed.Close()
	root, err := openStorageAccess(t.Context(), sdk.StorageNativeAccess{Descriptor: int(borrowed.Fd()),
		Family: sdk.StorageFamily{ID: "db", Primary: "history.db", Sidecars: []string{"fifo"}, Trees: []string{"media"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	flags, err := unix.FcntlInt(root.keep.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("owned root descriptor can cross exec", flags, err)
	}
	for _, name := range []string{"media/escape/secret", "fifo"} {
		if file, err := root.Open(name); err == nil {
			file.Close()
			t.Fatal("unsafe family component was opened", name)
		}
	}
	if err := os.Rename(directory, directory+".pinned"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Rename(directory+".pinned", directory) })
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(directory) })
	file, err := root.Open("history.db")
	if err != nil {
		t.Fatal("root was reopened through its replaced pathname", err)
	}
	body, err := io.ReadAll(file)
	file.Close()
	if err != nil || string(body) != "original" {
		t.Fatal("root identity changed after replacement", string(body), err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := borrowed.Stat(); err != nil {
		t.Fatal("root close consumed the borrowed host descriptor", err)
	}
	if file, err := root.Open("history.db"); err == nil {
		file.Close()
		t.Fatal("closed root remained readable")
	}
}
