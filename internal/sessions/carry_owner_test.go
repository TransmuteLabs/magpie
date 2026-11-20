package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	dir := t.TempDir()
	path := filepath.Join(dir, "converted.jsonl")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	checks := 0
	_, err = extension.PublishSessionDocument(root, filepath.Base(path), func(writer io.Writer) error {
		_, err := io.WriteString(writer, "private conversion\n")
		return err
	}, func(map[string]fs.FileInfo, bool) error {
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

func TestCarryLinkedContractUsesDeclaredStore(t *testing.T) {
	carrySetup(t)
	source, ok := Find("claude", carryClaude)
	if !ok {
		t.Fatal("missing source session")
	}
	original := linkedSessionConverters[0]
	t.Cleanup(func() { linkedSessionConverters[0] = original })
	linkedSessionConverters[0].Descriptor.OutputDir = "alternate-rollouts"
	result, err := CarryOver(source, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Path, filepath.Join(os.Getenv("HOME"), "alternate-rollouts")+string(filepath.Separator)) {
		t.Fatal("conversion ignored the selected descriptor output directory", result.Path)
	}
	if body := sum(t, result.Path); !strings.Contains(body, result.ID) {
		t.Fatal("missing converted session identity")
	}
}

func TestCarryLinkedContractFencesChangedCodexProfile(t *testing.T) {
	carrySetup(t)
	source, ok := Find("claude", carryClaude)
	if !ok {
		t.Fatal("missing source session")
	}
	original := linkedSessionConverters[0]
	t.Cleanup(func() { linkedSessionConverters[0] = original })
	linkedSessionConverters[0].Convert = func(ctx context.Context, input sdk.SessionConversionInput) (sdk.SessionConversionOutput, error) {
		if input.TargetProvider != "magpie" {
			t.Fatal("unexpected original provider", input.TargetProvider)
		}
		path := filepath.Join(CodexDir(), "config.toml")
		if err := os.WriteFile(path, []byte("profile = \"new\"\n[profiles.new]\nmodel_provider = \"second\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return original.Convert(ctx, input)
	}
	before := rollouts(t, CodexDir())
	if result, err := CarryOver(source, "codex"); !errors.Is(err, extension.ErrActionChanged) || result != (CarryResult{}) {
		t.Fatal("published using a superseded provider", result, err)
	}
	if after := rollouts(t, CodexDir()); !slices.Equal(after, before) {
		t.Fatal("rejected conversion left a rollout", after)
	}
	linkedSessionConverters[0] = original
	result, err := CarryOver(source, "codex")
	if err != nil {
		t.Fatal(err)
	}
	meta := readLines(t, result.Path)[0]["payload"].(map[string]any)
	if meta["model_provider"] != "second" {
		t.Fatal("retry did not use the current profile", meta)
	}
}

func TestCarryLinkedPublicationRejectsSameBytesReplacement(t *testing.T) {
	carrySetup(t)
	source, ok := Find("claude", carryClaude)
	if !ok {
		t.Fatal("missing source session")
	}
	before := rollouts(t, CodexDir())
	var replaced string
	var independent fs.FileInfo
	sourceBeforePublish = func() {
		paths := slices.DeleteFunc(rollouts(t, CodexDir()), func(name string) bool { return slices.Contains(before, name) })
		if len(paths) == 0 {
			return
		}
		if len(paths) != 1 {
			t.Fatal("unexpected new rollouts", paths)
		}
		replaced = paths[0]
		temp, err := os.CreateTemp(filepath.Dir(replaced), "independent-*.tmp")
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.WriteString(temp, sum(t, replaced))
		if err := errors.Join(err, temp.Close()); err != nil {
			t.Fatal(err)
		}
		independent, err = os.Stat(temp.Name())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temp.Name(), replaced); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { sourceBeforePublish = nil })
	if result, err := CarryOver(source, "codex"); !errors.Is(err, extension.ErrActionChanged) || result != (CarryResult{}) || replaced == "" {
		t.Fatal("reported an independent file as its result", result, err, replaced)
	}
	sourceBeforePublish = nil
	success, err := CarryOver(source, "codex")
	if err != nil || success.Path == replaced {
		t.Fatal("rejected output was cached", success, err)
	}
	if after, err := os.Stat(replaced); err != nil || !os.SameFile(independent, after) {
		t.Fatal("independent file was removed or changed", err)
	}
}

func TestCarryLinkedContractRejectsFailedDocument(t *testing.T) {
	carrySetup(t)
	source, ok := Find("claude", carryClaude)
	if !ok {
		t.Fatal("missing source session")
	}
	original := linkedSessionConverters[0]
	t.Cleanup(func() { linkedSessionConverters[0] = original })
	failure := errors.New("converter ran out of output")
	calls, writes := 0, 0
	linkedSessionConverters[0].Convert = func(ctx context.Context, input sdk.SessionConversionInput) (sdk.SessionConversionOutput, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok || input.Cwd != carryCwd || input.TargetProvider != "magpie" {
			t.Fatal("missing conversion context or selected target metadata")
		}
		body, err := io.ReadAll(input.Source)
		if err != nil || !strings.Contains(string(body), carryClaude) {
			t.Fatal("converter did not receive selected source", err)
		}
		return sdk.SessionConversionOutput{
			ID: "0190aaaa-1111-7222-8333-444455556666", Path: "private-output.jsonl",
			Write: func(_ context.Context, writer io.Writer) error {
				writes++
				_, err := io.WriteString(writer, "partial document")
				return errors.Join(err, failure)
			},
		}, nil
	}
	if len(carries(source)) != 1 || calls != 0 {
		t.Fatal("listing invoked the converter")
	}
	root := filepath.Join(CodexDir(), "sessions")
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := CarryOver(source, "codex"); !errors.Is(err, failure) || result != (CarryResult{}) || calls != 1 || writes != 1 {
		t.Fatalf("CarryOver returned %+v, %v after %d calls and %d writes", result, err, calls, writes)
	}
	after, err := os.ReadDir(root)
	if err != nil || len(after) != len(before) {
		t.Fatal("partial document or temporary file remains in the target store", err)
	}
	linkedSessionConverters[0] = original
	if result, err := CarryOver(source, "codex"); err != nil || result.ID == "" {
		t.Fatal("failed document prevented a successful retry", result, err)
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
	for _, mutation := range []string{"owner", "consent", "edited", "replaced", "moved-parent", "moved-parent-revoked"} {
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
			var sentinel string
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
				if mutation == "moved-parent" || mutation == "moved-parent-revoked" {
					parent := filepath.Dir(output)
					moved := parent + ".moved"
					sentinel = filepath.Join(parent, "independent.txt")
					if err := os.WriteFile(sentinel, []byte("independent output"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(parent, moved); err != nil {
						t.Fatal(err)
					}
					output = filepath.Join(moved, filepath.Base(output))
					sentinel = filepath.Join(moved, filepath.Base(sentinel))
				}
				switch mutation {
				case "consent", "moved-parent-revoked":
					foreignHistoryConsentOff(t)
				case "owner", "edited", "replaced":
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
				t.Fatal("revoked carry left private output", output, err)
			}
			temporary, err := filepath.Glob(filepath.Join(filepath.Dir(output), ".magpie-extension-*.tmp"))
			if err != nil || len(temporary) != 0 {
				t.Fatal("revoked carry left temporary output", temporary, err)
			}
			if sentinel != "" {
				if body, err := os.ReadFile(sentinel); err != nil || string(body) != "independent output" {
					t.Fatal("rollback changed an unrelated file", string(body), err)
				}
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
