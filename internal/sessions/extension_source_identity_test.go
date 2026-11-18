package sessions

import (
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSessionSourceGrantBindsPhysicalRootAcrossReopen(t *testing.T) {
	setup(t)
	parent := t.TempDir()
	root := filepath.Join(parent, "sessions")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte("{\"id\":\"same-session\",\"text\":\"same bytes\"}\n")
	write := func() {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "session.jsonl"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	descriptor := sdk.SessionSourceDescriptor{ID: "history", Agent: "fixture-source"}
	store := sdk.SessionSourceStore{Slot: "main", HomeRelativePath: "sessions"}
	grant := func() sourceGrant {
		t.Helper()
		result, err := makeSourceGrant("fixture", "same-package", descriptor, store, root)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first, reopened := grant(), grant()
	if first.reference != reopened.reference {
		t.Fatal("reopening the same physical source root changed its authority")
	}
	if err := os.Rename(root, filepath.Join(parent, "previous")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	write()
	replacement := grant()
	if os.SameFile(first.info, replacement.info) {
		t.Fatal("fixture did not replace the physical directory")
	}
	if replacement.reference == first.reference {
		t.Fatal("same-path same-bytes replacement retained a previous source root grant")
	}
}
