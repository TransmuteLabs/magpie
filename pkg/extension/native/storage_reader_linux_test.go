package native

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
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
