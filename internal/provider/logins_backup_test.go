package provider

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/yetone/magpie/internal/lastgood"
	"github.com/yetone/magpie/internal/steady"
)

func loginBackupContents(t *testing.T, file string) []byte {
	t.Helper()
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestLoginsKeepsCurrentPrimaryBeforeReplacing(t *testing.T) {
	for _, scenario := range []string{"mode", "symlink", "corrupt", "replaced_after_draft"} {
		t.Run(scenario, func(t *testing.T) {
			if runtime.GOOS == "windows" && (scenario == "mode" || scenario == "symlink") {
				t.Skip("requires Unix file modes and symlinks")
			}
			signIn(t)
			row := savedLogin{Agent: "codex", User: "me@example.com", Plan: "alpha"}
			saveLogins(t, row)
			path := loginsPath()
			backup := lastgood.Bak(path)
			original := loginBackupContents(t, path)
			want := bytes.Replace(original, []byte(`"alpha"`), []byte(`"omega"`), 1)
			wantBackup := original
			row.Plan = "omega"

			switch scenario {
			case "mode":
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
				row.Plan, want = "alpha", original
			case "symlink":
				target := filepath.Join(filepath.Dir(path), "linked-logins.json")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Base(target), path); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(backup, original, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, make([]byte, len(original)), 0o600); err != nil {
					t.Fatal(err)
				}
			case "replaced_after_draft":
				wantBackup = bytes.Replace(original, []byte(`"alpha"`), []byte(`"delta"`), 1)
				peer := path + ".peer"
				if err := os.WriteFile(peer, wantBackup, 0o600); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(peer, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				before := beforeLoginsWrite
				t.Cleanup(func() { beforeLoginsWrite = before })
				beforeLoginsWrite = func() {
					if err := os.Rename(peer, path); err != nil {
						t.Fatal(err)
					}
				}
			}

			saveLogins(t, row)
			if !bytes.Equal(loginBackupContents(t, path), want) || !bytes.Equal(loginBackupContents(t, backup), wantBackup) {
				t.Fatal("primary or last-good generation differs")
			}
			if scenario == "symlink" {
				if target, err := os.Readlink(path); err != nil || target != "linked-logins.json" {
					t.Fatalf("writer replaced the symlink: %q, %v", target, err)
				}
			}
			info, err := os.Stat(path)
			if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
				t.Fatalf("primary was not restored to private mode: %v, %v", info, err)
			}
			if scenario == "corrupt" {
				names, err := filepath.Glob(path + ".bad-*")
				if err != nil || len(names) != 1 || !bytes.Equal(loginBackupContents(t, names[0]), make([]byte, len(original))) {
					t.Fatalf("the corrupt primary was not kept aside: %v, %v", names, err)
				}
			}
		})
	}
}

func TestLoginsRechecksAfterKeepingBackup(t *testing.T) {
	for _, scenario := range []string{"peer_published_same_bytes", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			signIn(t)
			row := savedLogin{Agent: "codex", User: "me@example.com", Plan: "alpha"}
			saveLogins(t, row)
			path := loginsPath()
			backup := lastgood.Bak(path)
			original := loginBackupContents(t, path)
			expected := bytes.Replace(original, []byte(`"alpha"`), []byte(`"omega"`), 1)
			row.Plan = "omega"
			peer := path + ".peer"
			if err := os.WriteFile(peer, expected, 0o600); err != nil {
				t.Fatal(err)
			}
			peerInfo, err := os.Stat(peer)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			syncFile := steady.Sync
			t.Cleanup(func() { steady.Sync = syncFile })
			backupFlushed := false
			steady.Sync = func(file *os.File) error {
				if err := syncFile(file); err != nil {
					return err
				}
				if !stagedWriteFor(file, backup) {
					return nil
				}
				backupFlushed = true
				if scenario == "canceled" {
					cancel()
					return nil
				}
				return os.Rename(peer, path)
			}

			stamp := StampNow()
			loginsMu.Lock()
			err = writeLoginsChecked([]savedLogin{row}, true, ctx.Err)
			loginsMu.Unlock()
			if !backupFlushed || !bytes.Equal(loginBackupContents(t, backup), original) {
				t.Fatal("fixture did not keep the previous primary")
			}
			if scenario == "canceled" {
				if !errors.Is(err, context.Canceled) || !bytes.Equal(loginBackupContents(t, path), original) || StampNow() != stamp {
					t.Fatalf("canceled write published logins: %v", err)
				}
			} else {
				current, statErr := os.Stat(path)
				if err != nil || statErr != nil || !os.SameFile(current, peerInfo) ||
					!bytes.Equal(loginBackupContents(t, path), expected) {
					t.Fatalf("writer replaced the peer's equal bytes: %v, %v", err, statErr)
				}
			}
		})
	}
}
