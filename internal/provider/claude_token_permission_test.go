package provider

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestClaudeOwnedTokenRepeatRepairsSettingsPermissions(test *testing.T) {
	if runtime.GOOS == "windows" {
		test.Skip("no Unix modes")
	}
	for _, action := range []string{"setter", "ordinary switch"} {
		for _, target := range []string{"regular", "symlink"} {
			test.Run(action+"/"+target, func(test *testing.T) {
				claudeThree(test)
				writeFile(test, claudeSettingsFile(), map[string]any{
					"env": map[string]any{"KEEP": "unchanged"}, "model": "opus",
					"permissions": map[string]any{"allow": []string{"Read"}},
				})
				user, err := AddClaudeSetupToken(testSetupToken)
				if err != nil {
					test.Fatal(err)
				}
				if err := SwitchLogin("claude", user); err != nil {
					test.Fatal(err)
				}
				path := claudeSettingsFile()
				if target == "symlink" {
					backing := filepath.Join(test.TempDir(), "settings-target.json")
					if err := os.Rename(path, backing); err != nil {
						test.Fatal(err)
					}
					if err := os.Symlink(backing, path); err != nil {
						test.Fatal(err)
					}
				}
				if err := os.Chmod(path, 0o644); err != nil {
					test.Fatal(err)
				}
				old := time.Now().Add(-time.Hour).Truncate(time.Second)
				if err := os.Chtimes(path, old, old); err != nil {
					test.Fatal(err)
				}
				paths := map[string]string{
					"settings": path, "ownership record": claudeTokenRecordPath(),
					"saved logins":       loginsPath(),
					"stored credentials": filepath.Join(filepath.Dir(path), ".credentials.json"),
				}
				snapshots := make(map[string][]byte, len(paths))
				for name, file := range paths {
					content, err := os.ReadFile(file)
					if err != nil {
						test.Fatal(err)
					}
					snapshots[name] = content
				}
				before, err := os.Stat(path)
				if err != nil {
					test.Fatal(err)
				}
				if action == "setter" {
					err = setClaudeEnvToken(testSetupToken)
				} else {
					err = SwitchLogin("claude", user)
				}
				if err != nil {
					test.Fatal(err)
				}
				after, err := os.Stat(path)
				if err != nil {
					test.Fatal(err)
				}
				if after.Mode().Perm() != 0o600 {
					test.Errorf("repeating %s leaves owned token settings mode %#o, want 0600", action, after.Mode().Perm())
				}
				if !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
					test.Error("idempotent permission repair rewrote or replaced settings")
				}
				for name, file := range paths {
					content, err := os.ReadFile(file)
					if err != nil {
						test.Fatal(err)
					}
					if !bytes.Equal(content, snapshots[name]) {
						test.Errorf("%s bytes changed while repeating the same owned token", name)
					}
				}
				if token, exists := envTokenOf(test); !exists || token != testSetupToken {
					test.Error("owned token value changed")
				}
				if !claudeLoginOf(test, user).Uses {
					test.Error("repeat Use lost the selected setup-token identity")
				}
				if target == "symlink" {
					info, err := os.Lstat(path)
					if err != nil {
						test.Fatal(err)
					}
					if info.Mode()&os.ModeSymlink == 0 {
						test.Error("permission repair replaced the user's settings symlink")
					}
				}
			})
		}
	}
}

func TestClaudeForeignTokenRepeatLeavesSettingsPermissions(test *testing.T) {
	if runtime.GOOS == "windows" {
		test.Skip("no Unix modes")
	}
	for _, action := range []string{"setter", "ordinary switch"} {
		for _, ownership := range []string{"unowned same value", "user replaced owned value"} {
			test.Run(action+"/"+ownership, func(test *testing.T) {
				claudeThree(test)
				user, err := AddClaudeSetupToken(testSetupToken)
				if err != nil {
					test.Fatal(err)
				}
				foreign := testSetupToken
				if ownership == "user replaced owned value" {
					if err := SwitchLogin("claude", user); err != nil {
						test.Fatal(err)
					}
					foreign = "sk-ant-oat01-user-controlled-synthetic-token"
				}
				writeFile(test, claudeSettingsFile(), map[string]any{
					"env":   map[string]any{claudeTokenEnvKey: foreign, "KEEP": "unchanged"},
					"model": "opus",
				})
				path := claudeSettingsFile()
				if err := os.Chmod(path, 0o644); err != nil {
					test.Fatal(err)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					test.Fatal(err)
				}
				recordBefore, recordBeforeErr := os.ReadFile(claudeTokenRecordPath())
				if recordBeforeErr != nil && !os.IsNotExist(recordBeforeErr) {
					test.Fatal(recordBeforeErr)
				}
				if action == "setter" {
					err = setClaudeEnvToken(testSetupToken)
				} else {
					err = SwitchLogin("claude", user)
				}
				if err == nil || !strings.Contains(err.Error(), "of your own") {
					test.Errorf("foreign token replacement was not refused: %v", err)
				}
				after, err := os.ReadFile(path)
				if err != nil {
					test.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					test.Error("foreign token or unrelated settings changed")
				}
				info, err := os.Stat(path)
				if err != nil {
					test.Fatal(err)
				}
				if info.Mode().Perm() != 0o644 {
					test.Errorf("foreign settings mode changed to %#o", info.Mode().Perm())
				}
				recordAfter, recordAfterErr := os.ReadFile(claudeTokenRecordPath())
				if recordAfterErr != nil && !os.IsNotExist(recordAfterErr) {
					test.Fatal(recordAfterErr)
				}
				if os.IsNotExist(recordBeforeErr) != os.IsNotExist(recordAfterErr) || !bytes.Equal(recordBefore, recordAfter) {
					test.Error("foreign token ownership record changed")
				}
			})
		}
	}
}
