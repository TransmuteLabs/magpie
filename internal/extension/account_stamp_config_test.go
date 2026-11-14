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

func TestAccountStampObservesOnlyRootModuleConfiguration(t *testing.T) {
	root := t.TempDir()
	before, err := accountStampAt(root)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	scope := sdk.Scope{Plugin: "fixture.config"}
	if _, err := store.StateGet(scope, moduleConfigKey); err != nil {
		t.Fatal(err)
	}
	absent, err := accountStampAt(root)
	if err != nil || !absent.Equal(before) {
		t.Fatal("reading absent configuration changed the source stamp", err)
	}
	if _, err := store.StateCAS(scope, "unrelated", 0, json.RawMessage(`{"value":1}`)); err != nil {
		t.Fatal(err)
	}
	unrelated, err := accountStampAt(root)
	if err != nil || !unrelated.Equal(before) {
		t.Fatal("unrelated private state changed the source stamp", err)
	}
	if _, err := store.StateCAS(scope, moduleConfigKey, 0, json.RawMessage(`{"enabled":false}`)); err != nil {
		t.Fatal(err)
	}
	configured, err := accountStampAt(root)
	if err != nil || configured.Equal(before) {
		t.Fatal("root module configuration CAS did not invalidate the source stamp", err)
	}
	account, err := store.SaveAccount(Account{Plugin: scope.Plugin, Provider: "vendor", ID: "alice",
		Auth: json.RawMessage(`{"token":"synthetic"}`)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	withAccount, err := accountStampAt(root)
	if err != nil {
		t.Fatal(err)
	}
	accountScope := sdk.Scope{Plugin: scope.Plugin, Provider: account.Provider, Account: account.ID, Generation: account.Generation}
	if _, err := store.StateCAS(accountScope, moduleConfigKey, 0, json.RawMessage(`{"accountPrivate":true}`)); err != nil {
		t.Fatal(err)
	}
	unchanged, err := accountStampAt(root)
	if err != nil || !unchanged.Equal(withAccount) {
		t.Fatal("account-local state changed the root configuration stamp", err)
	}
}

func TestAccountStampConfigReplacementRetainsIdentityFence(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	scope := sdk.Scope{Plugin: "fixture.config"}
	if _, err := store.StateCAS(scope, moduleConfigKey, 0, json.RawMessage(`{"mode":"old"}`)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "private", scope.Plugin, "state", storeStateName(scope, moduleConfigKey))
	original, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	info, err := original.Stat()
	if err != nil {
		t.Fatal(err)
	}
	before, err := accountStampAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(root).StateCAS(scope, moduleConfigKey, 1, json.RawMessage(`{"mode":"new"}`)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(path)
	if err != nil || os.SameFile(info, current) || current.Size() != info.Size() ||
		current.Mode() != info.Mode() || !current.ModTime().Equal(info.ModTime()) {
		t.Fatal("fixture did not atomically replace config at identical metadata", err)
	}
	after, err := accountStampAt(root)
	if err != nil || after.Equal(before) {
		t.Fatal("same-metadata configuration replacement retained the source stamp", err)
	}
}

func TestAccountStampRefusesUnavailableConfigurationMetadata(t *testing.T) {
	for _, kind := range []string{"state-file", "config-directory", "config-symlink", "state-symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			namespace := filepath.Join(root, "private", "fixture.config")
			if err := os.MkdirAll(namespace, 0o700); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(namespace, "state")
			if kind == "state-file" {
				if err := os.WriteFile(state, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if kind == "state-symlink" {
				if runtime.GOOS == "windows" {
					t.Skip("symlink creation requires Windows privileges")
				}
				if err := os.Symlink(t.TempDir(), state); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(state, 0o700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(state, storeStateName(sdk.Scope{Plugin: "fixture.config"}, moduleConfigKey))
				if kind == "config-directory" {
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
			if _, err := accountStampAt(root); err == nil {
				t.Fatal("unavailable configuration metadata compared as unchanged")
			}
		})
	}
}
