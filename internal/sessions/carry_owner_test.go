package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func delegateHistoryForTest(t *testing.T, agent string) {
	t.Helper()
	body, err := json.Marshal([]SessionSourceBinding{{Plugin: "new-owner", Source: "history", Agent: agent, Identity: strings.Repeat("a", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	writeHistoryAuthority(t, sourceBindingsPath(), body)
}

func TestCarryPublicationPreservesCompetingOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "converted.jsonl")
	checks := 0
	err := writeNew(path, [][]byte{[]byte("private conversion")}, func() error {
		checks++
		if checks == 2 {
			return os.WriteFile(path, []byte("independent output"), 0o600)
		}
		return nil
	})
	if err == nil {
		t.Fatal("conversion replaced a competing output")
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "independent output" {
		t.Fatal("conversion damaged competing output", string(body), err)
	}
}

func TestConverterPublicationCleanupPreservesEditedPeer(t *testing.T) {
	setup(t)
	source := filepath.Join(t.TempDir(), "source.jsonl")
	writeLines(t, source, `{"type":"user","uuid":"p","message":{"role":"user","content":"private prompt"}}`)
	host := &sessionActionHost{entries: []extension.Installed{{Manifest: sdk.Manifest{
		ID: "external", Version: "1.0.0", Contract: sdk.FullContract(),
		Sessions: []sdk.SessionDescriptor{{ID: "conversation", Name: "Conversation"}},
	}}}, outputs: []sdk.SessionDocument{
		{Path: "one.jsonl", Format: "external", Body: []byte("private one")},
		{Path: "two.jsonl", Format: "external", Body: []byte("private two")},
	}}
	root := t.TempDir()
	policy := extension.ActionPolicy{Root: root, CreateUnder: "sessions", BackupDir: filepath.Join(t.TempDir(), "backups")}
	plan, _, err := convertExtensionSource(t.Context(), host, "external", "conversation",
		Session{Agent: "claude", ID: "selected", Path: source, Cwd: "/work"}, "external", sdk.Scope{}, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	first, second := filepath.Join(root, "sessions/one.jsonl"), filepath.Join(root, "sessions/two.jsonl")
	result, err := plan.ExecuteCreateOnly(t.Context(), plan.Preview().Revision, func(map[string]fs.FileInfo) error {
		if err := os.WriteFile(first, []byte("independent edit"), 0o600); err != nil {
			t.Fatal(err)
		}
		delegateHistoryForTest(t, "claude")
		return nil
	})
	if result.Applied || !errors.Is(err, extension.ErrActionChanged) {
		t.Fatal("changed output was not reported", err)
	}
	if body, err := os.ReadFile(first); err != nil || string(body) != "independent edit" {
		t.Fatal("cleanup deleted independently edited output", string(body), err)
	}
	if _, err := os.Stat(second); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cleanup stranded unchanged private output beside edited file", err)
	}
}

func TestCarryPublicationRevocationCleansOnlyNewOutput(t *testing.T) {
	for _, mutation := range []string{"owner", "consent", "edited", "replaced"} {
		t.Run(mutation, func(t *testing.T) {
			d := consentHome(t)
			path := writeTwoAgents(t, d)
			writeLines(t, path, `{"type":"user","uuid":"p","timestamp":"2026-11-13T00:00:00Z","message":{"role":"user","content":"my secret plan"},"cwd":"/work/app","sessionId":"sess1"}`,
				claudeMsg("m1", "claude-opus-5-5", 100, 1, 0, 0, 1))
			if err := SetReads(nil, map[string]bool{"claude": true}); err != nil {
				t.Fatal(err)
			}
			s, ok := Find("claude", "sess1")
			if !ok {
				t.Fatal("missing fixture")
			}
			previous := agentThere
			agentThere = func(string) bool { return true }
			t.Cleanup(func() { agentThere = previous; sourceBeforePublish = nil })
			pattern := filepath.Join(CodexDir(), "sessions", "*", "*", "*", "rollout-*.jsonl")
			existing, err := filepath.Glob(pattern)
			if err != nil {
				t.Fatal(err)
			}
			var output string
			sourceBeforePublish = func() {
				files, err := filepath.Glob(pattern)
				files = slices.DeleteFunc(files, func(path string) bool { return slices.Contains(existing, path) })
				// agentVersion performs an ordinary list before any write.
				if err == nil && len(files) == 0 {
					return
				}
				if err != nil || len(files) != 1 {
					t.Fatal("conversion publication did not produce exactly one new output", files, err)
				}
				output = files[0]
				if mutation == "consent" {
					foreignHistoryConsentOff(t)
				} else {
					delegateHistoryForTest(t, "claude")
				}
				if mutation == "replaced" {
					if err := os.Rename(output, output+".original"); err != nil {
						t.Fatal(err)
					}
				}
				if mutation == "edited" || mutation == "replaced" {
					if err := os.WriteFile(output, []byte("independent output"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			result, err := CarryOver(s, "codex")
			if err == nil || result != (CarryResult{}) || output == "" {
				t.Fatal("revoked carry published its new output", result, err)
			}
			body, err := os.ReadFile(output)
			if mutation == "edited" || mutation == "replaced" {
				if err != nil || string(body) != "independent output" {
					t.Fatal("rollback deleted independent output", string(body), err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal("revoked carry left private output", string(body), err)
			}
			temporary, _ := filepath.Glob(filepath.Join(filepath.Dir(output), ".rollout-*"))
			if len(temporary) != 0 {
				t.Fatal("revoked carry left temporary output", temporary)
			}
		})
	}
}

func TestCarryCachedResultRechecksOwner(t *testing.T) {
	d := consentHome(t)
	path := writeTwoAgents(t, d)
	writeLines(t, path, `{"type":"user","uuid":"p","timestamp":"2026-11-13T00:00:00Z","message":{"role":"user","content":"my secret plan"},"cwd":"/work/app","sessionId":"sess1"}`,
		claudeMsg("m1", "claude-opus-5-5", 100, 1, 0, 0, 1))
	if err := SetReads(nil, map[string]bool{"claude": true}); err != nil {
		t.Fatal(err)
	}
	s, ok := Find("claude", "sess1")
	if !ok {
		t.Fatal("missing native fixture")
	}
	previous := agentThere
	agentThere = func(string) bool { return true }
	t.Cleanup(func() { agentThere = previous })
	first, err := CarryOver(s, "codex")
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	agentThere = func(string) bool {
		once.Do(func() { delegateHistoryForTest(t, "claude") })
		return true
	}
	if result, err := CarryOver(s, "codex"); !errors.Is(err, extension.ErrActionChanged) || result != (CarryResult{}) {
		t.Fatal("cached native output published after takeover", result, err)
	}
	if _, err := os.Stat(first.Path); err != nil {
		t.Fatal("revocation deleted an already published output", err)
	}
}

func TestConverterRetainsHistoryOwner(t *testing.T) {
	for _, phase := range []string{"during-plan", "before-apply", "after-write", "after-write-edited"} {
		t.Run(phase, func(t *testing.T) {
			setup(t)
			source := filepath.Join(t.TempDir(), "source.jsonl")
			writeLines(t, source, `{"type":"user","uuid":"p","message":{"role":"user","content":"private prompt"}}`)
			selected := Session{Agent: "claude", ID: "selected", Path: source, Cwd: "/work"}
			host := &sessionActionHost{entries: []extension.Installed{{Manifest: sdk.Manifest{
				ID: "external", Version: "1.0.0", Contract: sdk.FullContract(),
				Sessions: []sdk.SessionDescriptor{{ID: "conversation", Name: "Conversation"}},
			}}}}
			root := t.TempDir()
			output := filepath.Join(root, "converted.jsonl")
			var once sync.Once
			revoke := func() { once.Do(func() { delegateHistoryForTest(t, "claude") }) }
			if phase == "during-plan" {
				host.duringInvoke = revoke
			}
			policy := extension.ActionPolicy{Root: root, Paths: []string{"converted.jsonl"}, BackupDir: filepath.Join(t.TempDir(), "backups")}
			if strings.HasPrefix(phase, "after-write") {
				policy.RevalidateInputs = func(context.Context, map[string]fs.FileInfo) error {
					if _, err := os.Stat(output); err == nil {
						if phase == "after-write-edited" {
							if err := os.WriteFile(output, []byte("independent edit"), 0o600); err != nil {
								t.Fatal(err)
							}
						}
						revoke()
					}
					return nil
				}
			}
			plan, _, err := convertExtensionSource(t.Context(), host, "external", "conversation", selected, "external", sdk.Scope{}, nil, policy)
			if phase != "during-plan" {
				if err != nil {
					t.Fatal(err)
				}
				if phase == "before-apply" {
					revoke()
				}
				_, err = plan.Execute(t.Context(), plan.Preview().Revision)
			}
			if !errors.Is(err, extension.ErrActionChanged) {
				t.Fatal("converter published under a different history owner", err)
			}
			body, err := os.ReadFile(output)
			if phase == "after-write-edited" {
				if err != nil || string(body) != "independent edit" {
					t.Fatal("rollback removed independently edited output", string(body), err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal("revoked conversion left output", string(body), err)
			}
		})
	}
}
