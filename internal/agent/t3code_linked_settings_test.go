package agent

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filememo"
)

func TestT3CodeSyncKeepsFreshSettings(t *testing.T) {
	home := syncHome(t)
	t.Setenv("T3CODE_HOME", "")
	agent := t3code(home)
	const input = `{
  "providerInstances": {
    "claudeAgent": {"config":{"binaryPath":"/opt/old/claude"}},
    "magpie": {"displayName":"previous", "enabled":false, "config":{"launchArgs":"--previous"}},
    "magpie-codex": {"displayName":"Codex"}
  }
}`
	writeFile(t, agent.Path, input)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(agent.Path, old, old); err != nil {
		t.Fatal(err)
	}
	release := filememo.Hold()
	defer release()
	if got, _ := edit.GetJSON(agent.Path, t3Instance+".displayName"); got != "previous" {
		t.Fatalf("fixture provider name: %q", got)
	}
	user := strings.NewReplacer("previous", "latest", "/opt/old/", "/opt/user/").Replace(input)
	writeFile(t, agent.Path, user)
	if got, _ := edit.GetJSON(agent.Path, t3Instance+".displayName"); got != "previous" {
		t.Fatalf("fixture lost its held value: %q", got)
	}
	if err := agent.Sync(); err != nil {
		t.Fatal(err)
	}
	release()
	for key, want := range map[string]string{
		"displayName":       "latest",
		"enabled":           "false",
		"config.launchArgs": "--latest",
		"config.binaryPath": "/opt/user/claude",
	} {
		if got, _ := edit.GetJSON(agent.Path, t3Instance+"."+key); got != want {
			t.Errorf("Sync lost the current %s: got %q, want %q", key, got, want)
		}
	}
}

func TestT3CodeSyncRollsBackCatalogOnSettingsConflict(t *testing.T) {
	home := syncHome(t)
	t.Setenv("T3CODE_HOME", "")
	agent := t3code(home)
	const input = `{"providerInstances":{"magpie-codex":{"config":{"launchArgs":"--previous"}}}}`
	const user = `{"providerInstances":{"magpie-codex":{"displayName":"User change","config":{"launchArgs":"--mine"}}}}`
	const catalog = `{"models":[]}`
	writeFile(t, agent.Path, input)
	catalogPath := t3CodexCatalog(agent.Path)
	writeFile(t, catalogPath, catalog)
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
	}, agent.Sync)
	if !changed || !errors.Is(err, extension.ErrActionChanged) {
		t.Fatalf("Sync did not reject the edited settings: changed=%t, err=%v", changed, err)
	}
	if got := readFile(agent.Path); got != user {
		t.Fatalf("user edit was overwritten: %s", got)
	}
	if got := readFile(catalogPath); got != catalog {
		t.Fatalf("catalog was published despite a rejected settings plan: %d bytes", len(got))
	}
}

func TestT3CodeSyncLeavesSettingsWhenCatalogIsBlocked(t *testing.T) {
	home := syncHome(t)
	t.Setenv("T3CODE_HOME", "")
	agent := t3code(home)
	const input = `{"providerInstances":{"magpie":{"enabled":true}}}`
	writeFile(t, agent.Path, input)
	if err := os.Mkdir(t3CodexCatalog(agent.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := agent.Sync(); err == nil {
		t.Fatal("Sync did not report the blocked catalog")
	}
	if got := readFile(agent.Path); got != input {
		t.Fatalf("settings were published despite a blocked catalog: %s", got)
	}
	if t3Stashed(t3CodexOffered) {
		t.Fatal("failed publication remembered a Codex instance")
	}
}

func TestT3CodeSyncSkipsCatalogAfterCodexRemoval(t *testing.T) {
	home := syncHome(t)
	t.Setenv("T3CODE_HOME", "")
	agent := t3code(home)
	writeFile(t, agent.Path, `{"providerInstances":{"magpie":{"displayName":"My Claude"}}}`)
	stash(map[string]string{t3CodexOffered: "1"})
	catalog := t3CodexCatalog(agent.Path)
	if err := os.Mkdir(catalog, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := agent.Sync(); err != nil {
		t.Fatal(err)
	}
	if got, _ := edit.GetJSON(agent.Path, t3Instance+".driver"); got != "claudeAgent" {
		t.Fatalf("Claude instance was not synced: %q", got)
	}
	if _, ok := edit.GetJSON(agent.Path, t3CodexInstance); ok {
		t.Fatal("Sync brought back the removed Codex provider")
	}
	if info, err := os.Stat(catalog); err != nil || !info.IsDir() {
		t.Fatalf("unused catalog path was changed: %v, %v", info, err)
	}
}
