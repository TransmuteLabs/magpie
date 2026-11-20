package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
)

func TestT3CodeConnectLeavesSettingsWhenCatalogIsBlocked(t *testing.T) {
	home := syncHome(t)
	t.Setenv("T3CODE_HOME", "")
	agent := t3code(home)
	const input = `{"providers":{"claudeAgent":{"binaryPath":"/user/claude"}}}`
	writeFile(t, agent.Path, input)
	if err := os.Mkdir(t3CodexCatalog(agent.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := agent.Connect(); err == nil {
		t.Fatal("Connect did not report the blocked catalog")
	}
	if got := readFile(agent.Path); got != input {
		t.Errorf("Connect changed settings despite a blocked catalog: %s", got)
	}
	if t3Stashed(t3CodexOffered) {
		t.Error("failed Connect remembered a Codex instance")
	}
}

func TestT3CodePickRollsBackNewCatalogOnSettingsConflict(t *testing.T) {
	home := syncHome(t)
	t.Setenv("T3CODE_HOME", "")
	agent := t3code(home)
	const before = `{"providerInstances":{"claudeAgent":{"config":{"binaryPath":"/user/claude"}}}}`
	const user = `{"providerInstances":{"claudeAgent":{"config":{"binaryPath":"/new/claude"}}}}`
	writeFile(t, agent.Path, before)
	target, err := edit.Target(agent.Path)
	if err != nil {
		t.Fatal(err)
	}
	var changed bool
	err = edit.Filtered(func(path string, body []byte) []byte {
		if path == target && !changed {
			writeFile(t, path, user)
			changed = true
		}
		return body
	}, func() error { return agent.Pick("provider", magpieID) })
	if !changed || !errors.Is(err, extension.ErrActionChanged) {
		t.Errorf("Pick did not reject changed settings: changed=%t, err=%v", changed, err)
	}
	if got := readFile(agent.Path); got != user {
		t.Errorf("Pick overwrote user settings: %s", got)
	}
	if _, err := os.Lstat(t3CodexCatalog(agent.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Pick left a catalog after rejected settings: %v", err)
	}
	if t3Stashed(t3CodexOffered) {
		t.Error("failed Pick remembered a Codex instance")
	}
}

func TestT3CodeDisconnectKeepsNewSettingsAndCatalogOnConflict(t *testing.T) {
	home := syncHome(t)
	t.Setenv("T3CODE_HOME", "")
	agent := t3code(home)
	const before = `{"providerInstances":{"magpie":{"displayName":"Earlier"},"magpie-codex":{}}}`
	const user = `{"providerInstances":{"magpie":{"displayName":"Renamed"},"magpie-codex":{}}}`
	const catalog = `{"models":[]}`
	catalogPath := t3CodexCatalog(agent.Path)
	writeFile(t, agent.Path, before)
	writeFile(t, catalogPath, catalog)
	stash(map[string]string{t3CodexOffered: "1"})
	target, err := edit.Target(agent.Path)
	if err != nil {
		t.Fatal(err)
	}
	var changed bool
	err = edit.Filtered(func(path string, body []byte) []byte {
		if path == target && !changed {
			writeFile(t, path, user)
			changed = true
		}
		return body
	}, agent.Disconnect)
	if !changed || !errors.Is(err, extension.ErrActionChanged) {
		t.Errorf("Disconnect did not reject changed settings: changed=%t, err=%v", changed, err)
	}
	if got := readFile(agent.Path); got != user {
		t.Errorf("Disconnect overwrote user settings: %s", got)
	}
	if got, err := os.ReadFile(catalogPath); err != nil || string(got) != catalog {
		t.Errorf("Disconnect lost the catalog: %s, %v", got, err)
	}
	if !t3Stashed(t3CodexOffered) {
		t.Error("failed Disconnect forgot the Codex instance")
	}
}

func TestT3CodeDisconnectUnlinksOnlyTheCatalogName(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		link     func(string, string) error
		dangling bool
	}{
		{name: "symlink", link: os.Symlink},
		{name: "hardlink", link: os.Link},
		{name: "dangling symlink", link: os.Symlink, dangling: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			home := syncHome(t)
			t.Setenv("T3CODE_HOME", "")
			agent := t3code(home)
			writeFile(t, agent.Path, `{"providerInstances":{"magpie-codex":{},"native":{}}}`)
			const catalog = `{"models":[]}`
			original := filepath.Join(filepath.Dir(agent.Path), "saved-catalog.json")
			if !fixture.dangling {
				writeFile(t, original, catalog)
			}
			path := t3CodexCatalog(agent.Path)
			if err := fixture.link(original, path); err != nil {
				t.Skipf("catalog link unavailable: %v", err)
			}
			if err := agent.Disconnect(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Disconnect left the catalog name: %v", err)
			}
			body, err := os.ReadFile(original)
			if fixture.dangling {
				if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("Disconnect created the missing target: %v", err)
				}
			} else if err != nil || string(body) != catalog {
				t.Errorf("Disconnect changed the other catalog name: %s, %v", body, err)
			}
			if _, exists := edit.GetJSON(agent.Path, "providerInstances.native"); !exists {
				t.Error("Disconnect removed the native provider")
			}
		})
	}
}
