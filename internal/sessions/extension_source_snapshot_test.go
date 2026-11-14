package sessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func sourceSnapshotFixture(t *testing.T) (sourceGrant, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "sess.jsonl")
	if err := os.WriteFile(path, []byte("{\"id\":\"sess\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	return sourceGrant{plugin: "fixture", identity: strings.Repeat("a", 64), root: root, reference: "store", info: info,
		descriptor: sdk.SessionSourceDescriptor{ID: "history", Agent: "fixture", Codec: "fixture-jsonl", Container: "jsonl",
			Layout: sdk.SessionSourceLayout{Kind: "flat", IDFrom: "filename-stem"}, Transcript: true, Resume: "none"}}, path
}

func TestSourceSnapshotUsesCheckedReader(t *testing.T) {
	for _, change := range []string{"content", "identity", "symlink", "parent", "root", "partial", "cancel"} {
		t.Run(change, func(t *testing.T) {
			grant, path := sourceSnapshotFixture(t)
			request, snapshot, err := captureSourceRequest(t.Context(), grant, "sess.jsonl", "summary")
			if err != nil {
				t.Fatal(err)
			}
			if request.Locator.SessionID != "sess" || len(request.Documents) != 1 {
				t.Fatal("selected source identity was lost")
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "content":
				if err := os.WriteFile(path, []byte("{\"id\":\"evil\"}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "identity":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, request.Documents[0].Body, 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".old", path); err != nil {
					t.Fatal(err)
				}
			case "parent", "root":
				if err := os.Rename(grant.root, grant.root+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(grant.root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, request.Documents[0].Body, 0o600); err != nil {
					t.Fatal(err)
				}
			case "partial":
				if err := os.WriteFile(path, []byte("{\"id\":\"sess\"}\n{\"id\":"), 0o600); err != nil {
					t.Fatal(err)
				}
				current, _, err := captureSourceRequest(t.Context(), grant, "sess.jsonl", "summary")
				if err != nil || string(current.Documents[0].Body) != "{\"id\":\"sess\"}\n" {
					t.Fatal("partial last entry was decoded", err)
				}
			case "cancel":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if _, _, err := captureSourceRequest(ctx, grant, "sess.jsonl", "summary"); !errors.Is(err, context.Canceled) {
					t.Fatal("canceled read proceeded", err)
				}
				return
			}
			if snapshot.check(t.Context()) == nil {
				t.Fatal("changed source was accepted")
			}
		})
	}
}

func TestSourceFiniteDiscoveryRejectsSymlinkAndTraversal(t *testing.T) {
	grant, path := sourceSnapshotFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "other.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "other.jsonl"), filepath.Join(grant.root, "other.jsonl")); err != nil {
		t.Fatal(err)
	}
	names, err := listSourceCandidates(t.Context(), grant)
	if err != nil || len(names) != 2 {
		t.Fatal("finite discovery failed", names, err)
	}
	if _, _, err := captureSourceRequest(t.Context(), grant, "other.jsonl", "summary"); err == nil {
		t.Fatal("source symlink was followed")
	}
	if _, _, err := captureSourceRequest(t.Context(), grant, "../"+filepath.Base(path), "summary"); err == nil {
		t.Fatal("traversal source was read")
	}
}
