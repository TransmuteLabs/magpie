package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestAccountStampDoesNotCreateStateOrReadCredentials(t *testing.T) {
	root := t.TempDir()
	empty, err := accountStampAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "private")); !os.IsNotExist(err) {
		t.Fatal("metadata read created private state")
	}
	store := NewStore(root)
	account, err := store.SaveAccount(Account{Plugin: "fixture", Provider: "vendor", ID: "alice",
		Auth: json.RawMessage(`{"token":"first"}`)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: account.Plugin, Provider: account.Provider, Account: account.ID, Generation: account.Generation}
	if _, err := store.StateCAS(scope, "fixture", 0, json.RawMessage(`{"value":1}`)); err != nil {
		t.Fatal(err)
	}
	first, err := accountStampAt(root)
	if err != nil || first.Equal(empty) {
		t.Fatal("first account did not change metadata stamp", err)
	}
	if _, err := store.StateCAS(scope, "fixture", 1, json.RawMessage(`{"value":2}`)); err != nil {
		t.Fatal(err)
	}
	unchanged, err := accountStampAt(root)
	if err != nil || !unchanged.Equal(first) {
		t.Fatal("unrelated private state changed account stamp", err)
	}
	account.Auth = json.RawMessage(`{"token":"other"}`)
	if _, err := NewStore(root).SaveAccount(account, account.Generation); err != nil {
		t.Fatal(err)
	}
	next, err := accountStampAt(root)
	if err != nil || next.Equal(first) {
		t.Fatal("same-size credential replacement retained account stamp", err)
	}
	if err := store.DeleteAccount(account.Plugin, account.Provider, account.ID, account.Generation+1); err != nil {
		t.Fatal(err)
	}
	deleted, err := accountStampAt(root)
	if err != nil || deleted.Equal(next) {
		t.Fatal("account deletion retained its old stamp", err)
	}
}

func TestAccountStampDetectsAtomicReplacementWithPreservedMetadata(t *testing.T) {
	root := t.TempDir()
	namespace := filepath.Join(root, "private", "fixture")
	if err := os.MkdirAll(namespace, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(namespace, "accounts.json")
	if err := os.WriteFile(path, []byte(`{"generation":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	original, err := file.Stat()
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	first, err := accountStampAt(root)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(namespace, "replacement")
	if err := os.WriteFile(replacement, []byte(`{"generation":2}`), original.Mode()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, time.Now(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(original, current) || current.Size() != original.Size() ||
		!current.ModTime().Equal(original.ModTime()) || current.Mode() != original.Mode() {
		t.Fatal("fixture did not replace the file while preserving stamp metadata")
	}
	next, err := accountStampAt(root)
	if err != nil || next.Equal(first) {
		t.Fatal("atomic replacement with preserved metadata retained account stamp", err)
	}
}

func TestAccountStampRejectsUnavailableNamespaceMetadata(t *testing.T) {
	for _, kind := range []string{"private-file", "namespace-file", "account-directory", "account-symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			private := filepath.Join(root, "private")
			if kind == "private-file" {
				if err := os.WriteFile(private, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(private, 0o700); err != nil {
					t.Fatal(err)
				}
				namespace := filepath.Join(private, "fixture")
				if kind == "namespace-file" {
					if err := os.WriteFile(namespace, nil, 0o600); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Mkdir(namespace, 0o700); err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(namespace, "accounts.json")
					if kind == "account-directory" {
						if err := os.Mkdir(path, 0o700); err != nil {
							t.Fatal(err)
						}
					} else {
						if runtime.GOOS == "windows" {
							t.Skip("symlink creation requires Windows privileges")
						}
						if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), path); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if _, err := accountStampAt(root); err == nil {
				t.Fatal("unavailable account metadata was treated as unchanged/empty")
			}
		})
	}
}
