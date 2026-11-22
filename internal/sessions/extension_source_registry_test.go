package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestBindHistoryOwnerHonorsCallerDeadline(t *testing.T) {
	t.Run("jsonl", func(t *testing.T) {
		setup(t)
		manager := sourceManager()
		directory := t.TempDir()
		manifest := sdk.Manifest{Schema: 1, ID: "fixture-bind-source", Version: "1.0.0", Runtime: "exec", Entry: "worker.exe",
			Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 6}, Capabilities: []sdk.Capability{sdk.SessionSource}},
			SessionSources: []sdk.SessionSourceDescriptor{{ID: "history", Agent: "claude", Codec: "fixture-jsonl", Container: "jsonl",
				Resume: "none", Stores: []sdk.SessionSourceStore{{Slot: "main", NativeRootSlot: "claude.projects"}},
				Layout: sdk.SessionSourceLayout{Kind: "flat", IDFrom: "filename-stem"}}}}
		body, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, manifest.Entry), []byte("binding must not start the worker"), 0o700); err != nil {
			t.Fatal(err)
		}
		entry, err := extension.NewCatalog(manager.Namespace()).Install(t.Context(), directory, func(context.Context, extension.Installed) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		path := sourceBindingsPath()
		writeHistoryAuthority(t, path, []byte("[]"))
		checkHistoryWriteCancellation(t, path, func(ctx context.Context) error {
			return BindSessionSourceNative(ctx, manifest.ID, "history", entry.Revision)
		})
		bindings, err := readSourceBindings()
		if err != nil || len(bindings) != 1 || bindings[0].Agent != "claude" || bindings[0].Plugin != manifest.ID || bindings[0].Source != "history" {
			t.Fatalf("retry did not publish the selected JSONL binding: %v, %v", bindings, err)
		}
	})
	t.Run("storage", func(t *testing.T) {
		if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
			t.Skip("native storage root identity requires Linux or Darwin")
		}
		_, owner := windowStorageFixture(t)
		roots, _, err := InspectStorageSources(t.Context())
		if err != nil || len(roots) != 2 {
			t.Fatalf("list bindable storage roots: %v, %v", roots, err)
		}
		root := roots[0]
		path := storageConsentsPath()
		writeHistoryAuthority(t, path, []byte("[]"))
		checkHistoryWriteCancellation(t, path, func(ctx context.Context) error {
			return BindStorageSource(ctx, owner.Module, root.Source, root.RootSlot, root.RootRef, root.Revision)
		})
		_, consents, err := InspectStorageSources(t.Context())
		if err != nil || len(consents) != 1 || consents[0].RootRef != root.RootRef || consents[0].RootSlot != root.RootSlot {
			t.Fatalf("retry did not publish consent for the selected storage root: %v, %v", consents, err)
		}
	})
}

func TestUnbindHistoryOwnerHonorsCallerDeadline(t *testing.T) {
	for _, storage := range []bool{false, true} {
		name, path := "jsonl", sourceBindingsPath
		if storage {
			name, path = "storage", storageConsentsPath
		}
		t.Run(name, func(t *testing.T) {
			setup(t)
			identity := strings.Repeat("a", 64)
			body, err := json.Marshal([]SessionSourceBinding{{Plugin: "fixture", Source: "history", Agent: "claude", Identity: identity}})
			if storage {
				body, err = json.Marshal([]StorageSourceConsent{{Plugin: "fixture", Source: "history", Agent: "claude", RootSlot: "main", RootRef: identity, Identity: identity}})
			}
			if err != nil {
				t.Fatal(err)
			}
			writeHistoryAuthority(t, path(), body)
			checkHistoryWriteCancellation(t, path(), func(ctx context.Context) error {
				return UnbindSessionSourceNative(ctx, "claude", identity)
			})
			after, err := os.ReadFile(path())
			if err != nil || string(after) != "[]" {
				t.Fatalf("retry did not remove the history owner: %s, %v", after, err)
			}
		})
	}
}

func checkHistoryWriteCancellation(t *testing.T, path string, operation func(context.Context) error) {
	t.Helper()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := func() {
		t.Helper()
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("canceled write changed history authority: %s -> %s (%v)", before, after, err)
		}
	}
	unlock, held := filelock.Lock(path+".lock", 0)
	if !held {
		t.Fatal("could not hold the history owner file lock")
	}
	release := sync.OnceFunc(unlock)
	defer release()
	deadline, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- operation(deadline) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("blocked write returned %v, want caller deadline", err)
		}
	case <-time.After(3 * time.Second):
		release()
		<-done
		t.Fatal("history write waited for the file lock after its deadline")
	}
	unchanged()
	release()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	staged := false
	err = edit.Filtered(func(target string, data []byte) []byte {
		if filepath.Base(target) == filepath.Base(path) {
			staged = true
			cancel()
		}
		return data
	}, func() error { return operation(ctx) })
	if !staged || !errors.Is(err, context.Canceled) {
		t.Errorf("write canceled during staging returned %v, reached staging %v", err, staged)
	}
	unchanged()
	if err := operation(t.Context()); err != nil {
		t.Fatalf("retry after cancellation: %v", err)
	}
}

func TestSourceBindingInspectionRejectsMalformedAuthority(t *testing.T) {
	setup(t)
	path := sourceBindingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, binding := range []SessionSourceBinding{
		{Plugin: "fixture", Source: "history", Agent: "omp", Identity: strings.Repeat("z", 64)},
		{Plugin: "fixture", Source: "history", Agent: "omp", Identity: strings.Repeat("A", 64)},
		{Plugin: "fixture", Source: "history", Agent: "foreign", Identity: strings.Repeat("a", 64)},
	} {
		body, err := json.Marshal([]SessionSourceBinding{binding})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		status := InspectSessionSources(context.Background())
		if len(status.Errors) == 0 || len(status.Bindings) != 0 {
			t.Fatal("source inspection treated malformed authority as a usable binding", status)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(body) {
			t.Fatal("failed inspection replaced malformed binding bytes", err)
		}
	}
}

func TestSourceUnknownFormatNeverUsesClaudeParser(t *testing.T) {
	setup(t)
	sourceView.Store(&sessionSourceView{epoch: sourceEpoch.Load(), agents: map[string]bool{"fixture-source": true}})
	defer sourceView.Store(nil)
	if parserFor("fixture-source").claude || parserFor("fixture-source").line != nil {
		t.Fatal("registered SDK source silently fell through to Claude")
	}
	path := filepath.Join(t.TempDir(), "source.jsonl")
	body := []byte("{\"type\":\"user\",\"sessionId\":\"secret\",\"cwd\":\"/secret\",\"message\":{\"content\":\"must not decode as Claude\"}}\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	item := file{agent: "fixture-source", key: "fixture-source:source", path: path, main: true}
	if !stat(&item) {
		t.Fatal("fixture missing")
	}
	state := parseRead(item, nil, nil)
	if state.ID != "" || state.Cwd != "" || state.Title != "" {
		t.Fatal("unknown codec was decoded using a native format", state)
	}
	if !parserFor("claude").claude {
		t.Fatal("SDK source quarantine changed native Claude default")
	}
}

func TestSourceRootSlotsRejectForeignOwners(t *testing.T) {
	setup(t)
	for _, slot := range []string{"claude.projects", "codex.sessions", "codex.archived-sessions", "omp.sessions", "devin.file-sessions"} {
		if _, err := sourceStoreRoots(sdk.SessionSourceDescriptor{Agent: "fixture-source"},
			sdk.SessionSourceStore{Slot: "main", NativeRootSlot: slot}); err == nil {
			t.Fatal("foreign native root was granted", slot)
		}
	}
}

func TestSourcePreparationDeadlineKeepsPublishedView(t *testing.T) {
	setup(t)
	catalog := extension.CatalogPath()
	if err := os.MkdirAll(filepath.Dir(catalog), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareSessionSources(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := sourceView.Load()
	if before == nil {
		t.Fatal("source preparation did not publish a view")
	}
	unlock, held := filelock.Lock(filepath.Join(filepath.Dir(catalog), "catalog.lock"), time.Second)
	if !held {
		t.Fatal("could not hold the SDK catalog lock")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- PrepareSessionSources(ctx) }()
	select {
	case err := <-done:
		unlock()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("source preparation returned %v instead of the deadline", err)
		}
	case <-time.After(3 * time.Second):
		unlock()
		<-done
		t.Fatal("source preparation waited for the catalog after the deadline")
	}
	if sourceView.Load() != before {
		t.Fatal("canceled discovery replaced the published source view")
	}
}
