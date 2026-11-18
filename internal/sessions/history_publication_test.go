package sessions

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/settings"
)

func TestHistoryPublicationSameStampConsent(t *testing.T) {
	d := consentHome(t)
	writeTwoAgents(t, d)
	if err := SetReads(nil, map[string]bool{"claude": true}); err != nil {
		t.Fatal(err)
	}
	before := []byte(`{"sessionReads":{"asked":true,"agents":{"claude":true }}}`)
	if err := os.WriteFile(settings.Path(), before, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(settings.Path(), old, old); err != nil {
		t.Fatal(err)
	}
	defer filememo.Hold()()
	first, version, _ := ListUnless(0, -1)
	if len(first) != 1 {
		t.Fatal("fixture missing", first)
	}
	after := bytes.Replace(before, []byte(`"claude":true `), []byte(`"claude":false`), 1)
	if err := os.WriteFile(settings.Path(), after, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(settings.Path(), old, old); err != nil {
		t.Fatal(err)
	}
	rows, _, same := ListUnless(0, version)
	if same || len(rows) != 0 {
		t.Fatal("stable denied snapshot published a held native listing", len(rows), same)
	}
	if _, err := TranscriptOf(first[0]); err == nil {
		t.Fatal("held session bypassed fresh consent")
	}
}

func TestHistoryPublicationWithoutSDKCatalog(t *testing.T) {
	d := consentHome(t)
	writeTwoAgents(t, d)
	if err := SetReads(nil, map[string]bool{"claude": true}); err != nil {
		t.Fatal(err)
	}
	previous := sourceManager
	sourceManager = func() *extension.Manager {
		t.Fatal("native history initialized SDK manager without catalog")
		return nil
	}
	t.Cleanup(func() { sourceManager = previous })
	if rows := ListAgent("claude"); len(rows) != 1 {
		t.Fatal("background/native history unavailable without catalog", len(rows))
	}
}

func TestHistoryPublicationSettingsWriter(t *testing.T) {
	if os.Getenv("MAGPIE_TEST_WRITE_CONSENT") != "1" {
		return
	}
	if err := settings.SetSessionReads(func(*settings.SessionReads) *settings.SessionReads {
		return &settings.SessionReads{Asked: true, Agents: map[string]bool{"claude": false}}
	}); err != nil {
		t.Fatal(err)
	}
}

func foreignHistoryConsentOff(t *testing.T) {
	t.Helper()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHistoryPublicationSettingsWriter$")
	command.Env = append(os.Environ(), "MAGPIE_TEST_WRITE_CONSENT=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("settings child: %v %s", err, output)
	}
}

// Exercise ordinary publication with the source epoch unchanged, as for
// a settings decision committed by a second process.
func TestHistoryPublicationForeignConsent(t *testing.T) {
	for _, operation := range []string{"list", "same", "get", "titles", "list-agent"} {
		t.Run(operation, func(t *testing.T) {
			d := consentHome(t)
			writeTwoAgents(t, d)
			if err := SetReads(nil, map[string]bool{"claude": true}); err != nil {
				t.Fatal(err)
			}
			initial, version, _ := ListUnless(0, -1)
			if !slices.ContainsFunc(initial, func(s Session) bool { return s.Agent == "claude" }) {
				t.Fatal("fixture has no native row")
			}
			epoch := sourceEpoch.Load()
			var once sync.Once
			sourceBeforePublish = func() {
				once.Do(func() {
					foreignHistoryConsentOff(t)
					if Reads("claude") || sourceEpoch.Load() != epoch {
						t.Fatal("failed to establish external-style revocation")
					}
				})
			}
			t.Cleanup(func() { sourceBeforePublish = nil })
			switch operation {
			case "list", "same":
				if operation == "list" {
					version = -1
				}
				out, _, same := ListUnless(0, version)
				if same || slices.ContainsFunc(out, func(s Session) bool { return s.Agent == "claude" }) {
					t.Errorf("revoked native history published: rows=%d same=%v readAllowed=%v", len(out), same, Reads("claude"))
				}
			case "get":
				out, found := Get("claude:sess1")
				if found {
					t.Errorf("revoked native Get published %q", out.Title)
				}
			case "titles":
				out := Titles([]string{"claude:sess1"})
				if out["claude:sess1"] != "" {
					t.Errorf("revoked native Titles published %q", out["claude:sess1"])
				}
			case "list-agent":
				if out := ListAgent("claude"); len(out) > 0 {
					t.Errorf("revoked native ListAgent published %d rows", len(out))
				}
			}
		})
	}
}

// The target availability hook is an existing ordinary-call seam. Commit a
// native ownership takeover there, after CarryOver's admission, before decode.
func TestCarryRetainsHistoryOwner(t *testing.T) {
	d := consentHome(t)
	sourcePath := writeTwoAgents(t, d)
	writeLines(t, sourcePath, `{"type":"user","uuid":"p","timestamp":"2026-11-13T00:00:00Z","message":{"role":"user","content":"my secret plan"},"cwd":"/work/app","sessionId":"sess1"}`,
		claudeMsg("m1", "claude-opus-5-5", 100, 1, 0, 0, 1))
	if err := SetReads(nil, map[string]bool{"claude": true}); err != nil {
		t.Fatal(err)
	}
	s, found := Find("claude", "sess1")
	if !found {
		t.Fatal("missing fixture")
	}
	old := agentThere
	var once sync.Once
	agentThere = func(string) bool {
		once.Do(func() {
			body, _ := json.Marshal([]SessionSourceBinding{{Plugin: "other-owner", Source: "history", Agent: "claude", Identity: strings.Repeat("a", 64)}})
			writeHistoryAuthority(t, sourceBindingsPath(), body)
		})
		return true
	}
	t.Cleanup(func() { agentThere = old })
	out, err := CarryOver(s, "codex")
	t.Logf("carry result=%+v err=%v", out, err)
	if _, check := TranscriptOf(s); check == nil {
		t.Fatal("fixture takeover did not revoke held native transcript")
	}
	if err == nil {
		body, readErr := os.ReadFile(out.Path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		t.Fatalf("CarryOver published %s after native takeover; copied private prompt=%v", filepath.Base(out.Path), strings.Contains(string(body), "my secret plan"))
	}
}

func TestDesktopTitlesForeignOwner(t *testing.T) {
	setup(t)
	data := t.TempDir()
	previous := callDesktopDirs
	callDesktopDirs = func() []string { return []string{data} }
	t.Cleanup(func() { callDesktopDirs = previous })
	const id = "11111111-aaaa-4000-8000-000000000001"
	writeDesktopTitle(t, filepath.Join(data, "claude-code-sessions", "account", "org", "local_desktop.json"),
		`{"cliSessionId":"`+id+`","title":"Private title before takeover"}`)
	if err := PrepareSessionSources(t.Context()); err != nil {
		t.Fatal(err)
	}
	if DesktopTitles([]string{id})[id] == "" {
		t.Fatal("fixture title missing")
	}
	epoch := sourceEpoch.Load()
	body, _ := json.Marshal([]SessionSourceBinding{{Plugin: "other-owner", Source: "history", Agent: "claude-desktop", Identity: strings.Repeat("a", 64)}})
	writeHistoryAuthority(t, sourceBindingsPath(), body)
	if sourceEpoch.Load() != epoch {
		t.Fatal("foreign-style takeover changed local epoch")
	}
	if got := DesktopTitles([]string{id}); got[id] != "" {
		t.Fatalf("ordinary DesktopTitles exposed revoked title %q after durable takeover", got[id])
	}
}

func TestDesktopTitlesRechecksOwnerAfterScan(t *testing.T) {
	setup(t)
	data := t.TempDir()
	previous := callDesktopDirs
	t.Cleanup(func() { callDesktopDirs = previous })
	const id = "11111111-aaaa-4000-8000-000000000001"
	writeDesktopTitle(t, filepath.Join(data, "claude-code-sessions", "account", "org", "local_desktop.json"),
		`{"cliSessionId":"`+id+`","title":"Private title during takeover"}`)
	callDesktopDirs = func() []string { delegateHistoryForTest(t, "claude-desktop"); return []string{data} }
	if got := DesktopTitles([]string{id}); got[id] != "" {
		t.Fatal("direct title lookup published after takeover during scan", got)
	}
}
