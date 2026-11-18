package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const (
	lineageRootID  = "11111111-1111-4111-8111-111111111111"
	lineageChildID = "22222222-2222-4222-8222-222222222222"
	lineageOtherID = "33333333-3333-4333-8333-333333333333"
)

type codexLineageTestHost struct {
	sessionInputHost
	planning sdk.SessionRequest
}

func (host *codexLineageTestHost) Invoke(ctx context.Context, plugin, method string, scope sdk.Scope, params, out any, consume func(extension.Event) error) error {
	if method == sdk.MethodModuleCall {
		if err := json.Unmarshal(params.(sdk.ModuleCallRequest).Input, &host.planning); err != nil {
			return err
		}
	}
	return host.sessionInputHost.Invoke(ctx, plugin, method, scope, params, out, consume)
}

func lineageRecord(test *testing.T, ordinal uint64, kind string, payload any) []byte {
	test.Helper()
	body, err := json.Marshal(map[string]any{
		"timestamp": "2026-11-15T12:00:00Z", "ordinal": ordinal, "type": kind, "payload": payload,
	})
	if err != nil {
		test.Fatal(err)
	}
	return append(body, '\n')
}

func lineageHeader(test *testing.T, id string, ordinal uint64, base any) []byte {
	test.Helper()
	return lineageRecord(test, ordinal, "session_meta", map[string]any{
		"id": id, "session_id": lineageRootID, "cwd": "/selected/work",
		"timestamp": "2026-11-15T12:00:00Z", "originator": "codex_cli_rs",
		"cli_version": "0.0.0", "source": "cli", "history_mode": "paginated",
		"history_base": base, "model_provider": "fixture-provider",
		"forked_from_id": lineageRootID, "forked_from_ordinal_exclusive": 3,
	})
}

func lineageMessage(test *testing.T, ordinal uint64, role, text string) []byte {
	test.Helper()
	part := "input_text"
	if role == "assistant" {
		part = "output_text"
	}
	return lineageRecord(test, ordinal, "response_item", map[string]any{
		"type": "message", "role": role, "content": []any{map[string]any{"type": part, "text": text}},
	})
}

func lineageBase(id string, ordinal uint64, offset int) map[string]any {
	return map[string]any{"thread_id": id, "end_ordinal_exclusive": ordinal, "end_byte_offset": offset}
}

func lineageWrite(test *testing.T, home, store, id string, body []byte) string {
	test.Helper()
	path := filepath.Join(home, store, "2026", "11", "15", "rollout-2026-11-15T12-00-00-"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		test.Fatal(err)
	}
	return path
}

func lineageFixture(test *testing.T) (Session, *codexLineageTestHost, string, []byte) {
	test.Helper()
	setup(test)
	home := filepath.Join(test.TempDir(), "codex")
	test.Setenv("CODEX_HOME", home)
	prefix := bytes.Join([][]byte{
		lineageHeader(test, lineageRootID, 0, nil),
		lineageMessage(test, 1, "user", "Inherited request"),
		lineageMessage(test, 2, "assistant", "Inherited answer"),
	}, nil)
	parent := lineageWrite(test, home, "sessions", lineageRootID,
		append(bytes.Clone(prefix), lineageMessage(test, 3, "user", "Excluded parent continuation")...))
	previousDay := filepath.Join(filepath.Dir(filepath.Dir(parent)), "14", filepath.Base(parent))
	if err := os.MkdirAll(filepath.Dir(previousDay), 0o700); err != nil {
		test.Fatal(err)
	}
	if err := os.Rename(parent, previousDay); err != nil {
		test.Fatal(err)
	}
	parent = previousDay
	child := append(lineageHeader(test, lineageChildID, 3, lineageBase(lineageRootID, 3, len(prefix))),
		lineageMessage(test, 4, "user", "Selected child suffix")...)
	path := lineageWrite(test, home, "sessions", lineageChildID, child)
	host := &codexLineageTestHost{sessionInputHost: sessionInputHost{
		sessionActionHost: sessionActionHost{entries: []extension.Installed{{Manifest: sdk.Manifest{
			ID: "external", Version: "1.0.0", Contract: sdk.FullContract(),
			Actions:  []sdk.ActionDescriptor{{ID: sessionInputsAction, Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			Sessions: []sdk.SessionDescriptor{{ID: "conversation", Name: "Conversation"}},
		}}}},
		plan: sessionInputPlan{Version: 1},
	}}
	host.entries[0].Manifest.Contract.Version.Minor = 10
	if !host.entries[0].Manifest.Contract.Has(sdk.SessionRelatedInputs) {
		host.entries[0].Manifest.Contract.Capabilities = append(host.entries[0].Manifest.Contract.Capabilities, sdk.SessionRelatedInputs)
	}
	return Session{Agent: "codex", ID: lineageChildID, Cwd: "/selected/work", Path: path}, host, parent, prefix
}

func TestCodexLineageCapturesManagedHistory(test *testing.T) {
	for _, layout := range []string{"plain", "packed-parent", "packed-child", "both-packed", "archived", "recursive", "reverted", "without-planner", "equal-copies", "partial-plain-twin", "linked-store", "selected-archived"} {
		test.Run(layout, func(test *testing.T) {
			source, host, parent, prefix := lineageFixture(test)
			switch layout {
			case "packed-parent", "both-packed":
				parent = pack(test, parent, false)
			case "archived":
				next := strings.Replace(parent, string(filepath.Separator)+"sessions"+string(filepath.Separator),
					string(filepath.Separator)+"archived_sessions"+string(filepath.Separator), 1)
				if err := os.MkdirAll(filepath.Dir(next), 0o700); err != nil {
					test.Fatal(err)
				}
				if err := os.Rename(parent, next); err != nil {
					test.Fatal(err)
				}
				parent = next
			case "recursive", "reverted":
				ancestorPrefix := bytes.Join([][]byte{
					lineageHeader(test, lineageOtherID, 0, nil),
					lineageMessage(test, 1, "user", "Inherited request"),
					lineageMessage(test, 2, "assistant", "Inherited answer"),
				}, nil)
				if layout == "reverted" {
					ancestorPrefix = prefix
					original := lineageWrite(test, CodexDir(), "archived_sessions", lineageRootID, ancestorPrefix)
					ancestor := strings.TrimSuffix(original, ".jsonl") + "_" + lineageOtherID + ".jsonl"
					if err := os.Rename(original, ancestor); err != nil {
						test.Fatal(err)
					}
				} else {
					lineageWrite(test, CodexDir(), "archived_sessions", lineageOtherID, ancestorPrefix)
				}
				delta := append(lineageHeader(test, lineageRootID, 3, lineageBase(lineageOtherID, 3, len(ancestorPrefix))),
					lineageMessage(test, 4, "assistant", "Intermediate inherited answer")...)
				if err := os.WriteFile(parent, delta, 0o600); err != nil {
					test.Fatal(err)
				}
				child := append(lineageHeader(test, lineageChildID, 5, lineageBase(lineageRootID, 5, len(delta))),
					lineageMessage(test, 6, "user", "Selected child suffix")...)
				if err := os.WriteFile(source.Path, child, 0o600); err != nil {
					test.Fatal(err)
				}
			case "without-planner":
				host.entries[0].Manifest.Actions = nil
			case "equal-copies":
				lineageWrite(test, CodexDir(), "archived_sessions", lineageRootID, prefix)
			case "partial-plain-twin":
				pack(test, parent, true)
				if err := os.WriteFile(parent, prefix[:len(prefix)-1], 0o600); err != nil {
					test.Fatal(err)
				}
			case "linked-store":
				root := filepath.Join(CodexDir(), "sessions")
				if err := os.Rename(root, root+".original"); err != nil {
					test.Fatal(err)
				}
				if err := os.Symlink(root+".original", root); err != nil {
					test.Fatal(err)
				}
			case "selected-archived":
				body, err := os.ReadFile(source.Path)
				if err != nil {
					test.Fatal(err)
				}
				next := lineageWrite(test, CodexDir(), "archived_sessions", lineageChildID, body)
				if err := os.Remove(source.Path); err != nil {
					test.Fatal(err)
				}
				source.Path = next
			}
			if layout == "packed-child" || layout == "both-packed" {
				source.Path = pack(test, source.Path, false)
			}
			original, err := os.ReadFile(source.Path)
			if err != nil {
				test.Fatal(err)
			}
			parentOriginal, err := os.ReadFile(parent)
			if err != nil {
				test.Fatal(err)
			}
			plan, snapshot, err := convertExtensionSource(test.Context(), host, "external", "conversation", source,
				"external", sdk.Scope{}, nil, inputPolicy(test))
			if err != nil {
				test.Fatal(err)
			}
			if len(host.request.Files) != 1 || host.request.Files[0].Format != "codex" {
				test.Fatalf("expected one complete native journal: %+v", host.request.Files)
			}
			related := host.request.Related
			if related == nil || related.Selected.RolloutID != lineageChildID ||
				!bytes.Contains(host.request.Files[0].Body, []byte(`"history_base":{`)) ||
				!bytes.Contains(host.request.Files[0].Body, []byte("Selected child suffix")) ||
				bytes.Contains(host.request.Files[0].Body, []byte("Inherited request")) {
				test.Fatal("core replaced the selected original with a materialized journal")
			}
			var inherited []byte
			for _, ancestor := range related.Ancestors {
				inherited = append(inherited, ancestor.Body...)
				if !bytes.Contains(ancestor.Body, []byte(`"type":"session_meta"`)) {
					test.Fatal("ancestor prefix lost its original metadata header")
				}
			}
			if !bytes.Contains(inherited, []byte("Inherited request")) ||
				!bytes.Contains(inherited, []byte("Inherited answer")) || bytes.Contains(inherited, []byte("Excluded parent")) {
				test.Fatal("carrier changed the inherited decoded prefixes or their exact cutoff")
			}
			if err := sdk.ValidateSessionRelatedInputs(host.entries[0].Manifest, host.request); err != nil {
				test.Fatal(err)
			}
			if layout != "without-planner" && !bytes.Contains(host.planning.Files[0].Body, []byte(`"history_base":{`)) {
				test.Fatal("v1 planner did not retain its original selected-file input")
			}
			if err := snapshot.check(test.Context()); err != nil {
				test.Fatal(err)
			}
			if _, err := plan.Execute(test.Context(), plan.Preview().Revision); err != nil {
				test.Fatal(err)
			}
			after, _ := os.ReadFile(source.Path)
			parentAfter, _ := os.ReadFile(parent)
			if !bytes.Equal(after, original) || !bytes.Equal(parentAfter, parentOriginal) {
				test.Fatal("acquisition modified an original rollout")
			}
		})
	}
}

func TestCodexLineageRejectsUnsafeHistory(test *testing.T) {
	for _, problem := range []string{"missing", "partial-record", "past-end", "ordinal", "cycle", "child-identity", "outside-home", "symlink-file", "symlink-directory", "divergent-copy", "malformed-base", "overflow", "missing-ordinal", "zero-cutoff", "overflow-byte", "compressed-truncated", "ancestor-gap", "foreign-store"} {
		test.Run(problem, func(test *testing.T) {
			source, host, parent, prefix := lineageFixture(test)
			base := lineageBase(lineageRootID, 3, len(prefix))
			switch problem {
			case "missing":
				if err := os.Remove(parent); err != nil {
					test.Fatal(err)
				}
			case "partial-record":
				base["end_byte_offset"] = len(prefix) - 1
			case "past-end":
				base["end_byte_offset"] = len(prefix) + 10000
			case "ordinal":
				base["end_ordinal_exclusive"] = 2
			case "cycle":
				base["thread_id"] = lineageChildID
			case "child-identity":
				source.ID = lineageOtherID
			case "outside-home":
				next := filepath.Join(test.TempDir(), filepath.Base(source.Path))
				if err := os.Rename(source.Path, next); err != nil {
					test.Fatal(err)
				}
				source.Path = next
			case "symlink-file", "symlink-directory":
				target := parent
				if problem == "symlink-directory" {
					target = filepath.Dir(parent)
				}
				if err := os.Rename(target, target+".original"); err != nil {
					test.Fatal(err)
				}
				if err := os.Symlink(target+".original", target); err != nil {
					test.Fatal(err)
				}
			case "divergent-copy":
				altered := bytes.ReplaceAll(prefix, []byte("Inherited answer"), []byte("Conflicting text"))
				lineageWrite(test, CodexDir(), "archived_sessions", lineageRootID, altered)
			case "malformed-base":
				base["thread_id"] = "../private"
			case "overflow":
				base["end_ordinal_exclusive"] = ^uint64(0)
			case "zero-cutoff":
				base["end_ordinal_exclusive"] = 0
			case "overflow-byte":
				base["end_byte_offset"] = ^uint64(0)
			case "compressed-truncated":
				packedParent := pack(test, parent, false)
				body, err := os.ReadFile(packedParent)
				if err != nil {
					test.Fatal(err)
				}
				if err := os.WriteFile(packedParent, body[:len(body)-4], 0o600); err != nil {
					test.Fatal(err)
				}
			case "ancestor-gap":
				prefix = bytes.ReplaceAll(prefix, []byte(`"ordinal":1,`), []byte(`"ordinal":9,`))
				if err := os.WriteFile(parent, prefix, 0o600); err != nil {
					test.Fatal(err)
				}
			case "foreign-store":
				body, err := os.ReadFile(parent)
				if err != nil {
					test.Fatal(err)
				}
				lineageWrite(test, test.TempDir(), "sessions", lineageRootID, body)
				if err := os.Remove(parent); err != nil {
					test.Fatal(err)
				}
			case "missing-ordinal":
				prefix = bytes.ReplaceAll(prefix, []byte(`"ordinal":1,`), nil)
				if err := os.WriteFile(parent, prefix, 0o600); err != nil {
					test.Fatal(err)
				}
				base["end_byte_offset"] = len(prefix)
			}
			if problem != "symlink-directory" && problem != "outside-home" {
				child := append(lineageHeader(test, lineageChildID, 3, base),
					lineageMessage(test, 4, "user", "Selected child suffix")...)
				if err := os.WriteFile(source.Path, child, 0o600); err != nil {
					test.Fatal(err)
				}
			}
			_, _, err := convertExtensionSource(test.Context(), host, "external", "conversation", source,
				"external", sdk.Scope{}, nil, inputPolicy(test))
			if err == nil || host.method == sdk.MethodSessionConvert {
				test.Fatalf("unsafe lineage reached the converter (%s): %v", problem, err)
			}
		})
	}
}

func TestCodexLineageFencesOriginalCaptures(test *testing.T) {
	for _, phase := range []string{"converting", "before-apply"} {
		for _, change := range []string{"contents", "identity", "append-excluded", "consent", "new-copy", "permissions", "directory-identity", "selected-contents"} {
			test.Run(phase+"/"+change, func(test *testing.T) {
				source, host, parent, prefix := lineageFixture(test)
				before, err := os.Stat(parent)
				if err != nil {
					test.Fatal(err)
				}
				mutate := func() {
					test.Helper()
					body, err := os.ReadFile(parent)
					if err != nil {
						test.Fatal(err)
					}
					switch change {
					case "contents":
						body = bytes.ReplaceAll(body, []byte("Inherited answer"), []byte("Different answer"))
					case "identity":
						if err := os.Rename(parent, parent+".original"); err != nil {
							test.Fatal(err)
						}
					case "append-excluded":
						body = append(body, lineageMessage(test, 4, "user", "Later parent append")...)
					case "consent":
						previous := OwnOff.Load()
						disabled := func() bool { return true }
						OwnOff.Store(&disabled)
						test.Cleanup(func() { OwnOff.Store(previous) })
						if err := os.Remove(source.Path); err != nil {
							test.Fatal(err)
						}
						return
					case "new-copy":
						lineageWrite(test, CodexDir(), "archived_sessions", lineageRootID, prefix)
						return
					case "permissions":
						if err := os.Chmod(parent, 0o400); err != nil {
							test.Fatal(err)
						}
						return
					case "directory-identity":
						directory := filepath.Dir(parent)
						if err := os.Rename(directory, directory+".original"); err != nil {
							test.Fatal(err)
						}
						if err := os.Mkdir(directory, 0o700); err != nil {
							test.Fatal(err)
						}
						entries, err := os.ReadDir(directory + ".original")
						if err != nil {
							test.Fatal(err)
						}
						for _, entry := range entries {
							if err := os.Link(filepath.Join(directory+".original", entry.Name()), filepath.Join(directory, entry.Name())); err != nil {
								test.Fatal(err)
							}
						}
						return
					case "selected-contents":
						selected, err := os.ReadFile(source.Path)
						if err != nil {
							test.Fatal(err)
						}
						selected = bytes.ReplaceAll(selected, []byte("Selected child suffix"), []byte("Replaced child suffix"))
						if err := os.WriteFile(source.Path, selected, 0o600); err != nil {
							test.Fatal(err)
						}
						return
					}
					if err := os.WriteFile(parent, body, 0o600); err != nil {
						test.Fatal(err)
					}
					if err := os.Chtimes(parent, before.ModTime(), before.ModTime()); err != nil {
						test.Fatal(err)
					}
				}
				if phase == "converting" {
					host.duringInvoke = mutate
				}
				policy := inputPolicy(test)
				plan, _, err := convertExtensionSource(test.Context(), host, "external", "conversation", source,
					"external", sdk.Scope{}, nil, policy)
				if phase == "before-apply" {
					if err != nil {
						test.Fatal(err)
					}
					mutate()
					_, err = plan.Execute(test.Context(), plan.Preview().Revision)
				}
				if err == nil {
					test.Fatal("changed original lineage capture or consent was accepted")
				}
				if change == "consent" && !errors.Is(err, ErrNotRead) {
					test.Fatalf("apply revalidation reopened a source after consent was revoked: %v", err)
				}
				if _, err := os.Stat(filepath.Join(policy.Root, "converted.jsonl")); !errors.Is(err, os.ErrNotExist) {
					test.Fatal("invalidated lineage published output")
				}
			})
		}
	}
}

func TestCodexLineageBounds(test *testing.T) {
	for _, limit := range []string{"depth", "decoded-bytes", "aggregate-bytes", "enumeration", "cancelled"} {
		test.Run(limit, func(test *testing.T) {
			source, host, parent, _ := lineageFixture(test)
			ctx := test.Context()
			switch limit {
			case "depth":
				if err := os.Remove(parent); err != nil {
					test.Fatal(err)
				}
				id, ordinal, prefix := lineageRootID, uint64(1), lineageHeader(test, lineageRootID, 0, nil)
				for index := 0; index < 34; index++ {
					lineageWrite(test, CodexDir(), "archived_sessions", id, prefix)
					next := fmt.Sprintf("44444444-4444-4444-8444-%012d", index)
					prefix = append(lineageHeader(test, next, ordinal, lineageBase(id, ordinal, len(prefix))),
						lineageMessage(test, ordinal+1, "user", "Inherited layer")...)
					id, ordinal = next, ordinal+2
				}
				lineageWrite(test, CodexDir(), "archived_sessions", id, prefix)
				if err := os.WriteFile(source.Path, lineageHeader(test, lineageChildID, ordinal, lineageBase(id, ordinal, len(prefix))), 0o600); err != nil {
					test.Fatal(err)
				}
			case "decoded-bytes":
				body := append(lineageHeader(test, lineageRootID, 0, nil),
					lineageMessage(test, 1, "user", strings.Repeat("x", extension.MaxActionBytes))...)
				if err := os.WriteFile(parent, body, 0o600); err != nil {
					test.Fatal(err)
				}
				pack(test, parent, false)
			case "aggregate-bytes":
				prefix := append(lineageHeader(test, lineageRootID, 0, nil),
					lineageMessage(test, 1, "user", strings.Repeat("x", 8<<20))...)
				if err := os.WriteFile(parent, prefix, 0o600); err != nil {
					test.Fatal(err)
				}
				body := append(lineageHeader(test, lineageChildID, 2, lineageBase(lineageRootID, 2, len(prefix))),
					lineageMessage(test, 3, "user", strings.Repeat("y", 8<<20))...)
				if err := os.WriteFile(source.Path, body, 0o600); err != nil {
					test.Fatal(err)
				}
			case "enumeration":
				directory := filepath.Join(CodexDir(), "archived_sessions")
				if err := os.MkdirAll(directory, 0o700); err != nil {
					test.Fatal(err)
				}
				for index := 0; index < 10001; index++ {
					if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("unrelated-%05d", index)), nil, 0o600); err != nil {
						test.Fatal(err)
					}
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			_, _, err := convertExtensionSource(ctx, host, "external", "conversation", source,
				"external", sdk.Scope{}, nil, inputPolicy(test))
			if err == nil || host.method == sdk.MethodSessionConvert {
				test.Fatalf("lineage exceeded %s: %v", limit, err)
			}
		})
	}
}

func TestCodexLineageDoesNotWidenInputPlanner(test *testing.T) {
	source, host, parent, _ := lineageFixture(test)
	body, err := os.ReadFile(parent)
	if err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(body)
	host.plan.Files = []sessionInputReference{{Path: "../14/" + filepath.Base(parent), SHA256: fmt.Sprintf("%x", digest)}}
	_, _, err = convertExtensionSource(test.Context(), host, "external", "conversation", source,
		"external", sdk.Scope{}, nil, inputPolicy(test))
	if err == nil || host.method == sdk.MethodSessionConvert {
		test.Fatal("native lineage resolution widened the relative v1 attachment authority")
	}
}
