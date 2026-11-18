package sessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func sourceDescendantFixture(t *testing.T) (sourceGrant, sdk.SessionSourceReadRequest, *sessionInputSnapshot) {
	t.Helper()
	grant, _ := sourceSnapshotFixture(t)
	grant.descriptor.Descendants = &sdk.SessionSourceDescendants{
		Kind: "journal-stem-jsonl", ExcludeDirectories: []string{"btw-history"},
	}
	for _, name := range []string{"sess/child.jsonl", "sess/child/advisor.jsonl", "sess/btw-history/ignored.jsonl",
		"sess/child/btw-history/ignored.jsonl", "other/child.jsonl"} {
		path := filepath.Join(grant.root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{\"id\":\"child\"}\n{\"partial\":"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	request, snapshot, err := captureSourceRequest(t.Context(), grant, "sess.jsonl", "summary")
	if err != nil {
		t.Fatal(err)
	}
	if err := collectSourceDescendants(t.Context(), grant, &request, snapshot); err != nil {
		t.Fatal(err)
	}
	return grant, request, snapshot
}

func TestSourceDescendantsUseSelectedCheckedTreeAndBudgets(t *testing.T) {
	grant, request, snapshot := sourceDescendantFixture(t)
	want := []string{"sess/child.jsonl", "sess/child/advisor.jsonl"}
	if !reflect.DeepEqual(request.Descendants, want) || len(request.Documents) != 3 || len(snapshot.files) != 3 {
		t.Fatal("automatic descendant acquisition lost selected tree or included unrelated documents", request.Descendants)
	}
	for _, document := range request.Documents[1:] {
		if string(document.Body) != "{\"id\":\"child\"}\n" {
			t.Fatal("unfinished child record was supplied to the decoder", document.Path)
		}
	}
	if err := os.WriteFile(filepath.Join(grant.root, "sess/btw-history/new.jsonl"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.check(t.Context()); err != nil {
		t.Fatal("excluded side history invalidated selected ordinary journal", err)
	}
	grant.descriptor.Descendants.ExcludeDirectories = append(grant.descriptor.Descendants.ExcludeDirectories, "child.jsonl")
	captured, checked, err := captureSourceRequest(t.Context(), grant, "sess.jsonl", "summary")
	if err != nil {
		t.Fatal(err)
	}
	if err := collectSourceDescendants(t.Context(), grant, &captured, checked); err != nil || !slices.Equal(captured.Descendants, want) {
		t.Fatal("a directory exclusion suppressed a regular child journal", captured.Descendants, err)
	}
	for _, mutation := range []string{"content", "identity", "new-child", "remove", "directory", "mode", "symlink"} {
		t.Run(mutation, func(t *testing.T) {
			grant, request, snapshot := sourceDescendantFixture(t)
			child := filepath.Join(grant.root, "sess/child.jsonl")
			info, err := os.Stat(child)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "content":
				body, _ := os.ReadFile(child)
				body = []byte(strings.Replace(string(body), "child", "other", 1))
				if err := os.WriteFile(child, body, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(child, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "identity":
				if err := os.Rename(child, child+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(child, request.Documents[1].Body, 0o600); err != nil {
					t.Fatal(err)
				}
			case "new-child":
				if err := os.WriteFile(filepath.Join(grant.root, "sess/new.jsonl"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "remove":
				if err := os.Remove(child); err != nil {
					t.Fatal(err)
				}
			case "directory":
				directory := filepath.Join(grant.root, "sess/child")
				if err := os.Rename(directory, directory+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(filepath.Join(grant.root, "sess/child"), 0o750); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(child, child+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(child+".old", child); err != nil {
					t.Fatal(err)
				}
			}
			if snapshot.check(t.Context()) == nil {
				t.Fatal("changed descendant snapshot retained authority")
			}
		})
	}
}

func TestSourceDescendantsInvalidateAbsentTreeAndRefuseUnsafeAcquisition(t *testing.T) {
	for _, mutation := range []string{"absent", "symlink-directory", "byte-budget", "file-budget", "depth", "cancel", "revoke"} {
		t.Run(mutation, func(t *testing.T) {
			grant, _ := sourceSnapshotFixture(t)
			grant.descriptor.Descendants = &sdk.SessionSourceDescendants{Kind: "journal-stem-jsonl"}
			request, snapshot, err := captureSourceRequest(t.Context(), grant, "sess.jsonl", "summary")
			if err != nil {
				t.Fatal(err)
			}
			if mutation == "absent" {
				if err := collectSourceDescendants(t.Context(), grant, &request, snapshot); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(grant.root, "sess"), 0o700); err != nil {
					t.Fatal(err)
				}
				if snapshot.check(t.Context()) == nil {
					t.Fatal("new descendant tree did not invalidate unchanged main journal")
				}
				return
			}
			directory := filepath.Join(grant.root, "sess")
			if mutation == "symlink-directory" {
				if err := os.Symlink(t.TempDir(), directory); err != nil {
					t.Fatal(err)
				}
			} else {
				if mutation == "depth" {
					directory = filepath.Join(directory, strings.Repeat("deep/", 18))
				}
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
				if mutation == "byte-budget" {
					if err := os.WriteFile(filepath.Join(directory, "big.jsonl"), make([]byte, extension.MaxActionBytes), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if mutation == "file-budget" {
					for index := range extension.MaxActionFiles {
						name := strings.Repeat("x", index+1) + ".jsonl"
						if err := os.WriteFile(filepath.Join(directory, name), []byte("{}\n"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			ctx := t.Context()
			if mutation == "cancel" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			if mutation == "revoke" {
				ctx = withSessionSourceAdmission(ctx, func() error { return ErrNotRead })
			}
			err = collectSourceDescendants(ctx, grant, &request, snapshot)
			if err == nil {
				t.Fatal("unsafe or over-budget descendant acquisition succeeded")
			}
			if mutation == "cancel" && !errors.Is(err, context.Canceled) ||
				mutation == "revoke" && !errors.Is(err, ErrNotRead) {
				t.Fatal("admission failure was not retained", err)
			}
		})
	}
}

func TestSourceDescendantIDsReachOrdinaryGatewayDedup(t *testing.T) {
	setup(t)
	ids := []sdk.SessionSourceIdentity{{Path: "sess/child.jsonl", SessionID: "child-id"},
		{Path: "sess/child/advisor.jsonl", SessionID: "advisor-id"}}
	record := &sessionSourceRecord{subs: sourceDescendantIDs(ids)}
	item := file{agent: "omp", key: "omp:main-id", path: "/synthetic/session", main: true}
	item.w().source = record
	subs := subIDs([]file{item}, "main-id")
	session := Session{Agent: "omp", ID: "main-id", Subs: subs}
	if !slices.Equal(GatewayKeys(session), []string{"omp|main-id", "omp|child-id", "omp|advisor-id"}) {
		t.Fatal("source descendants bypass ordinary gateway dedup", GatewayKeys(session))
	}
	subs[0] = "mutated"
	if record.subs[0] != "child-id" {
		t.Fatal("published descendant identities alias cached source metadata")
	}
}
