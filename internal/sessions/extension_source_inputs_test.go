package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type sessionInputHost struct {
	sessionActionHost
	plan       sessionInputPlan
	duringPlan func()
}

func (host *sessionInputHost) Invoke(ctx context.Context, plugin, method string, scope sdk.Scope, params, out any, consume func(extension.Event) error) error {
	if method != sdk.MethodModuleCall {
		return host.sessionActionHost.Invoke(ctx, plugin, method, scope, params, out, consume)
	}
	request := params.(sdk.ModuleCallRequest)
	if request.Action != sessionInputsAction {
		return errors.New("unexpected session input action")
	}
	if host.duringPlan != nil {
		host.duringPlan()
	}
	body, err := json.Marshal(host.plan)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func inputFixture(test *testing.T) (Session, *sessionInputHost, string, []byte) {
	test.Helper()
	setup(test)
	root := OmpDir()
	journal := filepath.Join(root, "sessions", "bucket", "selected.jsonl")
	if err := os.MkdirAll(filepath.Dir(journal), 0o700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte("synthetic journal"), 0o600); err != nil {
		test.Fatal(err)
	}
	body := []byte("synthetic image")
	digest := sha256.Sum256(body)
	hash := hex.EncodeToString(digest[:])
	blob := filepath.Join(root, "blobs", hash)
	if err := os.MkdirAll(filepath.Dir(blob), 0o700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(blob, body, 0o600); err != nil {
		test.Fatal(err)
	}
	host := &sessionInputHost{
		sessionActionHost: sessionActionHost{entries: []extension.Installed{{Manifest: sdk.Manifest{
			ID: "external", Version: "1.0.0", Contract: sdk.FullContract(),
			Actions:  []sdk.ActionDescriptor{{ID: sessionInputsAction, Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			Sessions: []sdk.SessionDescriptor{{ID: "conversation", Name: "Conversation"}},
		}}}},
		plan: sessionInputPlan{Version: 1, Files: []sessionInputReference{{Path: "blobs/" + hash, SHA256: hash}}},
	}
	return Session{Agent: "omp", ID: "selected", Cwd: "/work", Path: journal}, host, blob, body
}

func inputPolicy(test *testing.T) extension.ActionPolicy {
	test.Helper()
	return extension.ActionPolicy{Root: test.TempDir(), Paths: []string{"converted.jsonl"},
		BackupDir: filepath.Join(test.TempDir(), "backups")}
}

func TestSessionExtensionCollectsAttachmentSnapshots(test *testing.T) {
	source, host, blob, body := inputFixture(test)
	policy := inputPolicy(test)
	plan, snapshot, err := convertExtensionSource(test.Context(), host, "external", "conversation", source, "external", sdk.Scope{}, nil, policy)
	if err != nil {
		test.Fatal(err)
	}
	if len(host.request.Files) != 2 || host.request.Files[0].Path != "sessions/bucket/selected.jsonl" ||
		host.request.Files[1].Path != "blobs/"+filepath.Base(blob) || !bytes.Equal(host.request.Files[1].Body, body) ||
		host.request.Files[1].Format != source.Agent {
		test.Fatalf("selected store inputs were not supplied together: %+v", host.request.Files)
	}
	if err := snapshot.check(test.Context()); err != nil {
		test.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(policy.Root, "converted.jsonl")); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("input acquisition wrote conversion output")
	}
	if _, err := plan.Execute(test.Context(), plan.Preview().Revision); err != nil {
		test.Fatal(err)
	}
	after, err := os.ReadFile(blob)
	if err != nil || !bytes.Equal(after, body) {
		test.Fatal("attachment acquisition changed its source")
	}
}

func TestSessionExtensionAttachmentSourceChanges(test *testing.T) {
	for _, phase := range []string{"planning-inputs", "converting", "before-apply"} {
		for _, change := range []string{"contents", "identity"} {
			test.Run(phase+"/"+change, func(test *testing.T) {
				source, host, blob, body := inputFixture(test)
				info, err := os.Stat(blob)
				if err != nil {
					test.Fatal(err)
				}
				mutate := func() {
					test.Helper()
					if change == "identity" {
						if err := os.Rename(blob, blob+".original"); err != nil {
							test.Fatal(err)
						}
					} else {
						body = bytes.Clone(body)
						body[0] = 'X'
					}
					if err := os.WriteFile(blob, body, 0o600); err != nil {
						test.Fatal(err)
					}
					if err := os.Chtimes(blob, info.ModTime(), info.ModTime()); err != nil {
						test.Fatal(err)
					}
				}
				switch phase {
				case "planning-inputs":
					host.duringPlan = mutate
				case "converting":
					host.duringInvoke = mutate
				}
				_, snapshot, err := convertExtensionSource(test.Context(), host, "external", "conversation", source,
					"external", sdk.Scope{}, nil, inputPolicy(test))
				if phase == "before-apply" {
					if err != nil {
						test.Fatal(err)
					}
					mutate()
					err = snapshot.check(test.Context())
				}
				if phase == "planning-inputs" && change == "identity" {
					if err != nil {
						test.Fatal("identical bytes replaced before host capture must still be admissible:", err)
					}
					return
				}
				if err == nil {
					test.Fatal("changed source attachment was accepted")
				}
			})
		}
	}
}

func TestSessionExtensionAttachmentInputBounds(test *testing.T) {
	for _, problem := range []string{"traversal", "absolute", "digest", "duplicate", "journal", "symlink-file", "symlink-parent", "missing", "oversize", "version", "write-action"} {
		test.Run(problem, func(test *testing.T) {
			source, host, blob, _ := inputFixture(test)
			switch problem {
			case "traversal":
				host.plan.Files[0].Path = "../private"
			case "absolute":
				host.plan.Files[0].Path = blob
			case "digest":
				host.plan.Files[0].SHA256 = strings.Repeat("a", 64)
			case "duplicate":
				host.plan.Files = append(host.plan.Files, host.plan.Files[0])
			case "journal":
				host.plan.Files[0].Path = "sessions/bucket/selected.jsonl"
			case "symlink-file":
				if err := os.Rename(blob, blob+".target"); err != nil {
					test.Fatal(err)
				}
				if err := os.Symlink(blob+".target", blob); err != nil {
					test.Skipf("symlinks unavailable: %v", err)
				}
			case "symlink-parent":
				directory := filepath.Dir(blob)
				if err := os.Rename(directory, directory+".target"); err != nil {
					test.Fatal(err)
				}
				if err := os.Symlink(directory+".target", directory); err != nil {
					test.Skipf("symlinks unavailable: %v", err)
				}
			case "missing":
				if err := os.Remove(blob); err != nil {
					test.Fatal(err)
				}
			case "oversize":
				if err := os.Truncate(blob, extension.MaxActionBytes+1); err != nil {
					test.Fatal(err)
				}
			case "version":
				host.plan.Version = 2
			case "write-action":
				host.entries[0].Manifest.Actions[0].Effect = "write"
			}
			_, _, err := convertExtensionSource(test.Context(), host, "external", "conversation", source,
				"external", sdk.Scope{}, nil, inputPolicy(test))
			if err == nil || host.method == sdk.MethodSessionConvert {
				test.Fatalf("unsafe attachment plan reached conversion: %v", err)
			}
		})
	}
}

func TestSessionExtensionAttachmentStoreAndCancellation(test *testing.T) {
	source, host, _, _ := inputFixture(test)
	other := filepath.Join(test.TempDir(), "selected.jsonl")
	if err := os.WriteFile(other, []byte("other-store"), 0o600); err != nil {
		test.Fatal(err)
	}
	source.Path = other
	if _, _, err := convertExtensionSource(test.Context(), host, "external", "conversation", source,
		"external", sdk.Scope{}, nil, inputPolicy(test)); err == nil {
		test.Fatal("OMP attachment discovery escaped its native selected store")
	}
	ctx, cancel := context.WithCancel(test.Context())
	cancel()
	if _, _, err := readSessionInputs(ctx, filepath.Dir(other), "omp", filepath.Base(other),
		[]sessionInputReference{{Path: "asset", SHA256: strings.Repeat("a", 64)}}, 0); !errors.Is(err, context.Canceled) {
		test.Fatalf("attachment reading ignored cancellation: %v", err)
	}
}

func TestSessionExtensionAttachmentRetainsDirectoryIdentity(test *testing.T) {
	for _, replacement := range []string{"store", "blob-parent"} {
		test.Run(replacement, func(test *testing.T) {
			source, host, blob, _ := inputFixture(test)
			_, snapshot, err := convertExtensionSource(test.Context(), host, "external", "conversation", source,
				"external", sdk.Scope{}, nil, inputPolicy(test))
			if err != nil {
				test.Fatal(err)
			}
			directory := filepath.Dir(blob)
			if replacement == "store" {
				directory = OmpDir()
			}
			if err := os.Rename(directory, directory+".original"); err != nil {
				test.Fatal(err)
			}
			targets := []string{blob}
			if replacement == "store" {
				targets = append(targets, source.Path)
			}
			for _, target := range targets {
				relative, err := filepath.Rel(directory, target)
				if err != nil {
					test.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
					test.Fatal(err)
				}
				if err := os.Link(filepath.Join(directory+".original", relative), target); err != nil {
					test.Skipf("hardlinks unavailable: %v", err)
				}
			}
			if err := snapshot.check(test.Context()); !errors.Is(err, extension.ErrActionChanged) {
				test.Fatalf("same inode moved under a replaced %s was accepted: %v", replacement, err)
			}
		})
	}
}

func TestSessionExtensionPackedCodexSourceHasDecodedBudget(test *testing.T) {
	setup(test)
	path := filepath.Join(test.TempDir(), "source.jsonl")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), extension.MaxActionBytes+1), 0o600); err != nil {
		test.Fatal(err)
	}
	path = pack(test, path, false)
	host := &sessionActionHost{entries: []extension.Installed{{Manifest: sdk.Manifest{
		ID: "external", Version: "1.0.0", Contract: sdk.FullContract(),
		Sessions: []sdk.SessionDescriptor{{ID: "conversation", Name: "Conversation"}},
	}}}}
	_, _, err := convertExtensionSource(test.Context(), host, "external", "conversation",
		Session{Agent: "codex", ID: "selected", Cwd: "/work", Path: path}, "omp", sdk.Scope{}, nil, inputPolicy(test))
	if err == nil || host.method != "" {
		test.Fatal("packed source exceeded decoded budget or reached a worker before rejection")
	}
}

func TestSessionExtensionNativeTargetPrefixesBindCurrentStores(test *testing.T) {
	setup(test)
	home, _ := os.UserHomeDir()
	test.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	test.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	test.Setenv("XDG_DATA_HOME", "")
	for _, descriptor := range []sdk.SessionDescriptor{
		{To: "claude", OutputDir: ".claude/projects"},
		{To: "codex", OutputDir: ".codex/sessions"},
		{To: "omp", OutputDir: ".omp/agent/sessions"},
	} {
		if !extensionSessionTargetMatches(descriptor) {
			test.Fatalf("default native store rejected: %s", descriptor.To)
		}
	}
	test.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(test.TempDir(), "custom-claude"))
	test.Setenv("CODEX_HOME", filepath.Join(test.TempDir(), "custom-codex"))
	if !extensionSessionTargetMatches(sdk.SessionDescriptor{To: "claude", OutputDir: ".claude/projects"}) ||
		!extensionSessionTargetMatches(sdk.SessionDescriptor{To: "codex", OutputDir: ".codex/sessions"}) {
		test.Fatal("custom native store did not receive its host-owned binding")
	}
	if runtime.GOOS != "windows" {
		data := test.TempDir()
		if err := os.Mkdir(filepath.Join(data, "omp"), 0o700); err != nil {
			test.Fatal(err)
		}
		test.Setenv("XDG_DATA_HOME", data)
		if !extensionSessionTargetMatches(sdk.SessionDescriptor{To: "omp", OutputDir: ".omp/agent/sessions"}) {
			test.Fatal("migrated OMP data root did not receive its host-owned binding")
		}
	}
}
