package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func catalogTestPackage(t *testing.T, id, version string) string {
	t.Helper()
	source := t.TempDir()
	manifest := sdk.Manifest{
		Schema:  1,
		ID:      id,
		Version: version,
		Runtime: "exec",
		Entry:   "bin/worker",
		Args:    []string{"literal argument", "$(never-run)", "; echo nope"},
		Contract: sdk.Contract{
			Version:      sdk.APIVersion{Major: 1},
			Capabilities: []sdk.Capability{sdk.OperationControl},
		},
	}
	catalogTestWriteManifest(t, source, manifest)
	catalogTestWrite(t, filepath.Join(source, "bin", "worker"), []byte("executable fixture"), 0o755)
	catalogTestWrite(t, filepath.Join(source, ".hidden"), []byte("included"), 0o644)
	return source
}

func catalogTestWriteManifest(t *testing.T, source string, manifest sdk.Manifest) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	catalogTestWrite(t, filepath.Join(source, sdk.ManifestFile), data, 0o600)
}

func catalogTestWrite(t *testing.T, filename string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, data, mode); err != nil {
		t.Fatal(err)
	}
}

func catalogTestCheck(ctx context.Context, entry Installed) error {
	return ctx.Err()
}

func catalogTestInstall(t *testing.T, catalog *Catalog, source string) Installed {
	t.Helper()
	entry, err := catalog.Install(t.Context(), source, catalogTestCheck)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestCatalogInstallImmutableGenerations(t *testing.T) {
	catalog := NewCatalog(filepath.Join(t.TempDir(), "catalog"))
	source := catalogTestPackage(t, "sample", "1.0.0")
	var checked Installed
	first, err := catalog.Install(t.Context(), source, func(ctx context.Context, entry Installed) error {
		checked = entry
		if err := catalog.Verify(entry); err != nil {
			return err
		}
		listed, err := NewCatalog(catalog.Root).List()
		if err == nil && len(listed) != 0 {
			return errors.New("candidate published before the handshake")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, checked) {
		t.Fatal("handshake did not receive the frozen generation")
	}
	if filepath.IsAbs(first.Directory) || !strings.HasPrefix(first.Directory, "packages/sample/1.0.0-") {
		t.Fatalf("not a managed relative generation: %q", first.Directory)
	}
	if _, exists := first.Files[".hidden"]; !exists {
		t.Fatal("copy silently excluded a hidden distribution file")
	}
	if !reflect.DeepEqual(first.Manifest.Args, []string{"literal argument", "$(never-run)", "; echo nope"}) {
		t.Fatal("installation changed literal arguments")
	}
	catalogTestWrite(t, filepath.Join(source, "bin", "worker"), []byte("replacement generation"), 0o755)
	second := catalogTestInstall(t, NewCatalog(catalog.Root), source)
	if first.Directory == second.Directory || first.Revision == second.Revision {
		t.Fatal("same-version reinstallation reused an immutable generation")
	}
	if err := catalog.Verify(first); err != nil {
		t.Fatalf("old worker payload did not survive update: %v", err)
	}
	if err := catalog.Verify(second); err != nil {
		t.Fatal(err)
	}
	listed, err := catalog.List()
	if err != nil || len(listed) != 1 || listed[0].Revision != second.Revision {
		t.Fatalf("list after replacement: %+v, %v", listed, err)
	}
	if runtime.GOOS != "windows" {
		for _, name := range []string{catalog.Root, filepath.Join(catalog.Root, "packages"), filepath.Join(catalog.Root, first.Directory)} {
			info, err := os.Stat(name)
			if err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("directory must be private: %q, %v, %v", name, info, err)
			}
		}
		for name := range first.Files {
			info, err := os.Stat(filepath.Join(catalog.Root, first.Directory, name))
			if err != nil {
				t.Fatal(err)
			}
			want := os.FileMode(0o600)
			if name == first.Manifest.Entry {
				want = 0o700
			}
			if info.Mode().Perm() != want {
				t.Fatalf("%s permissions = %o, want %o", name, info.Mode().Perm(), want)
			}
		}
		info, err := os.Stat(filepath.Join(catalog.Root, "catalog.json"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("catalog must be private: %v, %v", info, err)
		}
	}
}

func TestCatalogManifestUsesSDKVersionAndArgumentRules(t *testing.T) {
	catalog := NewCatalog(t.TempDir())
	source := catalogTestPackage(t, "sample", "2.0.0")
	first := catalogTestInstall(t, catalog, source)
	manifest := first.Manifest
	manifest.Version = "1.0.0-preview.1+local"
	manifest.Args = make([]string, 20)
	for index := range manifest.Args {
		manifest.Args[index] = strings.Repeat("x", 60_000)
	}
	catalogTestWriteManifest(t, source, manifest)
	replacement := catalogTestInstall(t, catalog, source)
	if replacement.Manifest.Version != manifest.Version || !reflect.DeepEqual(replacement.Manifest.Args, manifest.Args) {
		t.Fatal("catalog added a version or argument policy beyond the SDK")
	}
	if err := catalog.Verify(first); err != nil {
		t.Fatalf("version replacement harmed its predecessor: %v", err)
	}
	if err := catalog.Verify(replacement); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogUpgradePreservesDisabled(t *testing.T) {
	catalog := NewCatalog(t.TempDir())
	source := catalogTestPackage(t, "sample", "1.0.0")
	first := catalogTestInstall(t, catalog, source)
	if err := catalog.SetEnabled(t.Context(), "sample", false); err != nil {
		t.Fatal(err)
	}
	manifest := first.Manifest
	manifest.Version = "2.0.0"
	catalogTestWriteManifest(t, source, manifest)
	replacement, err := catalog.Install(t.Context(), source, func(ctx context.Context, candidate Installed) error {
		if !candidate.Disabled {
			return errors.New("upgrade changed the user's disabled selection before its handshake")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := catalog.List()
	if err != nil || len(listed) != 1 || !listed[0].Disabled || !replacement.Disabled {
		t.Fatalf("upgrade reenabled a disabled module: %+v, %v", listed, err)
	}
	if replacement.Directory == first.Directory || replacement.Manifest.Version != "2.0.0" {
		t.Fatal("disabled module was not upgraded into a new immutable generation")
	}
	if err := catalog.Verify(first); err != nil {
		t.Fatal(err)
	}
}

func catalogTestOwnedPackage(t *testing.T, id, kind string) string {
	t.Helper()
	source := catalogTestPackage(t, id, "1.0.0")
	data, err := os.ReadFile(filepath.Join(source, sdk.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "path":
		manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.InboundCodec)
		manifest.Inbounds = []sdk.InboundDescriptor{{ID: "codec", Path: "/extension-shared"}}
	case "provider":
		manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.OutboundExecutor)
		manifest.Providers = []sdk.ProviderDescriptor{{ID: "shared", Name: "Shared", Protocols: []protocolext.Protocol{"chat"}}}
	case "agent":
		manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.AgentWiring)
		manifest.Agents = []sdk.AgentDescriptor{{ID: "editor", Name: "Editor"}}
	case "session":
		manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.SessionMigration)
		manifest.Sessions = []sdk.SessionDescriptor{{ID: "conversation", Name: "Conversation"}}
	default:
		t.Fatalf("unknown ownership fixture %q", kind)
	}
	catalogTestWriteManifest(t, source, manifest)
	return source
}

func TestCatalogEnableEnforcesGlobalOwnership(t *testing.T) {
	for _, kind := range []string{"path", "provider", "agent", "session"} {
		t.Run(kind, func(t *testing.T) {
			catalog := NewCatalog(t.TempDir())
			first := catalogTestInstall(t, catalog, catalogTestOwnedPackage(t, "first", kind))
			secondSource := catalogTestOwnedPackage(t, "second", kind)
			if _, err := catalog.Install(t.Context(), secondSource, catalogTestCheck); err == nil {
				t.Fatal("installation accepted conflicting enabled ownership")
			}
			if err := catalog.SetEnabled(t.Context(), first.Manifest.ID, false); err != nil {
				t.Fatal(err)
			}
			catalogTestInstall(t, catalog, secondSource)
			filename := filepath.Join(catalog.Root, "catalog.json")
			before, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}
			if err := catalog.SetEnabled(t.Context(), first.Manifest.ID, true); err == nil {
				t.Fatal("enable bypassed global ownership")
			}
			after, err := os.ReadFile(filename)
			if err != nil || string(after) != string(before) {
				t.Fatalf("rejected enable changed the catalog: %v", err)
			}
			entries, err := catalog.List()
			if err != nil || len(entries) != 2 || !entries[0].Disabled || entries[1].Disabled {
				t.Fatalf("ownership rejection changed enabled modules: %+v, %v", entries, err)
			}
			entries[0].Disabled = false
			data, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			catalogTestWrite(t, filename, data, 0o600)
			if entries, err := catalog.List(); err == nil || entries != nil {
				t.Fatalf("read accepted conflicting global ownership: %+v, %v", entries, err)
			}
		})
	}
}

func TestCatalogConcurrentEnablesClaimOwnershipOnce(t *testing.T) {
	catalog := NewCatalog(t.TempDir())
	for _, id := range []string{"first", "second"} {
		catalogTestInstall(t, catalog, catalogTestOwnedPackage(t, id, "path"))
		if err := catalog.SetEnabled(t.Context(), id, false); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, id := range []string{"first", "second"} {
		group.Go(func() {
			<-start
			results <- NewCatalog(catalog.Root).SetEnabled(t.Context(), id, true)
		})
	}
	close(start)
	group.Wait()
	close(results)
	successes, failures := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("concurrent enables did not claim ownership once: %d successes, %d failures", successes, failures)
	}
	entries, err := catalog.List()
	if err != nil {
		t.Fatal(err)
	}
	enabled := 0
	for _, entry := range entries {
		if !entry.Disabled {
			enabled++
		}
	}
	if enabled != 1 {
		t.Fatalf("catalog has %d enabled owners after concurrent enables", enabled)
	}
}

func TestCatalogDisableRemoveRetainsPayloadAndState(t *testing.T) {
	catalog := NewCatalog(t.TempDir())
	entry := catalogTestInstall(t, catalog, catalogTestPackage(t, "sample", "1.0.0"))
	state := filepath.Join(catalog.Root, "state", "sample", "recovery.json")
	catalogTestWrite(t, state, []byte(`{"operation":"uncertain"}`), 0o600)
	if err := catalog.SetEnabled(t.Context(), "sample", false); err != nil {
		t.Fatal(err)
	}
	listed, err := catalog.List()
	if err != nil || len(listed) != 1 || !listed[0].Disabled {
		t.Fatalf("disable was not persisted: %+v, %v", listed, err)
	}
	if err := catalog.Verify(entry); err != nil {
		t.Fatalf("disable prevented an existing generation draining: %v", err)
	}
	if err := catalog.SetEnabled(t.Context(), "sample", true); err != nil {
		t.Fatal(err)
	}
	listed, err = catalog.List()
	if err != nil || len(listed) != 1 || listed[0].Disabled || listed[0].Revision != entry.Revision {
		t.Fatalf("enable changed the frozen generation: %+v, %v", listed, err)
	}
	if err := catalog.Remove(t.Context(), "sample"); err != nil {
		t.Fatal(err)
	}
	listed, err = catalog.List()
	if err != nil || len(listed) != 0 {
		t.Fatalf("remove did not revoke discovery: %+v, %v", listed, err)
	}
	if err := catalog.Verify(entry); err != nil {
		t.Fatalf("remove deleted a draining worker's payload: %v", err)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("remove deleted recovery state: %v", err)
	}
	for _, change := range []func() error{
		func() error { return catalog.SetEnabled(t.Context(), "missing", true) },
		func() error { return catalog.Remove(t.Context(), "missing") },
	} {
		if err := change(); err == nil {
			t.Fatal("unknown id accepted")
		}
	}
}

func TestCatalogMissingAndBadReads(t *testing.T) {
	missing := NewCatalog(filepath.Join(t.TempDir(), "absent"))
	if entries, err := missing.List(); err != nil || entries == nil || len(entries) != 0 {
		t.Fatalf("absent catalog is not an empty list: %v, %v", entries, err)
	}
	if _, err := os.Stat(missing.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("listing a missing catalog created it: %v", err)
	}
	source := catalogTestPackage(t, "sample", "1.0.0")
	for _, data := range []string{"", "{", "null", "{}", "[{}]", "[] {}", `[{"unknown":true}]`} {
		t.Run(data, func(t *testing.T) {
			catalog := NewCatalog(t.TempDir())
			filename := filepath.Join(catalog.Root, "catalog.json")
			catalogTestWrite(t, filename, []byte(data), 0o600)
			if listed, err := catalog.List(); err == nil || listed != nil {
				t.Fatalf("bad read became a known list: %v, %v", listed, err)
			}
			checked := false
			if _, err := catalog.Install(t.Context(), source, func(context.Context, Installed) error {
				checked = true
				return nil
			}); err == nil || checked {
				t.Fatalf("installation did not refuse the unknown snapshot: %v, checked=%v", err, checked)
			}
			if err := catalog.SetEnabled(t.Context(), "sample", false); err == nil {
				t.Fatal("disable overwrote a malformed catalog")
			}
			if err := catalog.Remove(t.Context(), "sample"); err == nil {
				t.Fatal("remove overwrote a malformed catalog")
			}
			got, err := os.ReadFile(filename)
			if err != nil || string(got) != data {
				t.Fatalf("bad catalog was overwritten: %q, %v", got, err)
			}
		})
	}
	t.Run("directory", func(t *testing.T) {
		catalog := NewCatalog(t.TempDir())
		if err := os.Mkdir(filepath.Join(catalog.Root, "catalog.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		if listed, err := catalog.List(); err == nil || listed != nil {
			t.Fatalf("unreadable catalog became empty: %v, %v", listed, err)
		}
	})
	t.Run("permissions", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows does not use Unix mode bits for read access")
		}
		catalog := NewCatalog(t.TempDir())
		filename := filepath.Join(catalog.Root, "catalog.json")
		catalogTestWrite(t, filename, []byte("[]"), 0o600)
		if err := os.Chmod(filename, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(filename, 0o600); err != nil {
				t.Error(err)
			}
		})
		if _, err := os.ReadFile(filename); err == nil {
			t.Skip("this user can read files without read permission")
		}
		if listed, err := catalog.List(); err == nil || listed != nil {
			t.Fatalf("read permission failure became empty: %v, %v", listed, err)
		}
		if _, err := catalog.Install(t.Context(), source, catalogTestCheck); err == nil {
			t.Fatal("installation overwrote an unreadable catalog")
		}
	})
}

func TestCatalogStrictSnapshotValidation(t *testing.T) {
	for _, name := range []string{"duplicate-key", "unknown-field", "duplicate-id", "bad-revision", "truncated"} {
		t.Run(name, func(t *testing.T) {
			catalog := NewCatalog(t.TempDir())
			source := catalogTestPackage(t, "sample", "1.0.0")
			entry := catalogTestInstall(t, catalog, source)
			filename := filepath.Join(catalog.Root, "catalog.json")
			data, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "duplicate-key":
				data = []byte(strings.Replace(string(data), `"disabled":false`, `"disabled":false,"disabled":true`, 1))
			case "unknown-field":
				data = []byte(strings.Replace(string(data), `"disabled":false`, `"disabled":false,"future":true`, 1))
			case "duplicate-id":
				data, err = json.Marshal([]Installed{entry, entry})
			case "bad-revision":
				data = []byte(strings.Replace(string(data), entry.Revision, strings.Repeat("0", 64), 1))
			case "truncated":
				data = data[:len(data)-1]
			}
			if err != nil {
				t.Fatal(err)
			}
			catalogTestWrite(t, filename, data, 0o600)
			if listed, err := catalog.List(); err == nil || listed != nil {
				t.Fatalf("invalid snapshot accepted: %+v, %v", listed, err)
			}
			if _, err := catalog.Install(t.Context(), source, catalogTestCheck); err == nil {
				t.Fatal("installation accepted an invalid snapshot")
			}
			got, err := os.ReadFile(filename)
			if err != nil || string(got) != string(data) {
				t.Fatalf("invalid snapshot was overwritten: %q, %v", got, err)
			}
		})
	}
}

func TestCatalogFailedHandshakePreservesOldGeneration(t *testing.T) {
	catalog := NewCatalog(t.TempDir())
	source := catalogTestPackage(t, "sample", "1.0.0")
	first := catalogTestInstall(t, catalog, source)
	before, err := os.ReadFile(filepath.Join(catalog.Root, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var candidate Installed
	refused := errors.New("handshake descriptor mismatch")
	_, err = catalog.Install(t.Context(), source, func(ctx context.Context, entry Installed) error {
		candidate = entry
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("handshake error lost: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(catalog.Root, "catalog.json"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("failed handshake changed the catalog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(catalog.Root, candidate.Directory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed handshake left its candidate behind: %v", err)
	}
	if err := catalog.Verify(first); err != nil {
		t.Fatalf("failed handshake harmed previous generation: %v", err)
	}
	if _, err := catalog.Install(t.Context(), source, nil); err == nil {
		t.Fatal("installation without a handshake accepted")
	}
}

func TestCatalogHandshakeCannotRewriteFrozenMetadata(t *testing.T) {
	catalog := NewCatalog(t.TempDir())
	source := catalogTestPackage(t, "sample", "1.0.0")
	entry, err := catalog.Install(t.Context(), source, func(ctx context.Context, candidate Installed) error {
		candidate.Files["bin/worker"] = strings.Repeat("0", 64)
		candidate.Manifest.Args[0] = "rewritten"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Verify(entry); err != nil {
		t.Fatal(err)
	}
	if entry.Manifest.Args[0] == "rewritten" {
		t.Fatal("handshake mutated the catalog's frozen arguments")
	}
	_, err = catalog.Install(t.Context(), source, func(ctx context.Context, candidate Installed) error {
		return os.WriteFile(filepath.Join(catalog.Root, candidate.Directory, "bin", "worker"), []byte("tampered by handshake"), 0o700)
	})
	if err == nil {
		t.Fatal("payload modified during handshake was published")
	}
	listed, err := catalog.List()
	if err != nil || len(listed) != 1 || listed[0].Revision != entry.Revision {
		t.Fatalf("tampered candidate replaced the old entry: %v, %v", listed, err)
	}
}

func TestCatalogConcurrentInstallUsesSnapshotCAS(t *testing.T) {
	catalog := NewCatalog(t.TempDir())
	sources := []string{
		catalogTestPackage(t, "first", "1.0.0"),
		catalogTestPackage(t, "second", "1.0.0"),
	}
	type result struct {
		entry Installed
		err   error
	}
	results := make(chan result, 2)
	candidates := make(chan Installed, 2)
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	var group sync.WaitGroup
	defer func() {
		cancel()
		group.Wait()
	}()
	for _, source := range sources {
		group.Go(func() {
			entry, err := NewCatalog(catalog.Root).Install(ctx, source, func(ctx context.Context, candidate Installed) error {
				candidates <- candidate
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-release:
					return nil
				}
			})
			results <- result{entry, err}
		})
	}
	var staged []Installed
	for len(staged) < len(sources) {
		select {
		case candidate := <-candidates:
			staged = append(staged, candidate)
		case result := <-results:
			t.Fatalf("install returned before the handshake barrier: %v", result.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(release)
	group.Wait()
	close(results)
	var winner Installed
	conflicts := 0
	for result := range results {
		switch {
		case result.err == nil:
			winner = result.entry
		case errors.Is(result.err, ErrCatalogConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent result: %v", result.err)
		}
	}
	if winner.Manifest.ID == "" || conflicts != 1 {
		t.Fatalf("wanted one publication and one conflict: winner=%+v, conflicts=%d", winner, conflicts)
	}
	listed, err := catalog.List()
	if err != nil || len(listed) != 1 || listed[0].Revision != winner.Revision {
		t.Fatalf("stale writer clobbered the winner: %v, %v", listed, err)
	}
	for _, candidate := range staged {
		_, err := os.Stat(filepath.Join(catalog.Root, candidate.Directory))
		if candidate.Directory == winner.Directory {
			if err != nil {
				t.Fatalf("losing writer deleted the winner's generation: %v", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("losing writer left its candidate: %v", err)
		}
	}
}

func TestCatalogConcurrentDisableConflictsWithInstall(t *testing.T) {
	catalog := NewCatalog(t.TempDir())
	source := catalogTestPackage(t, "sample", "1.0.0")
	first := catalogTestInstall(t, catalog, source)
	_, err := catalog.Install(t.Context(), source, func(ctx context.Context, candidate Installed) error {
		return NewCatalog(catalog.Root).SetEnabled(ctx, "sample", false)
	})
	if !errors.Is(err, ErrCatalogConflict) {
		t.Fatalf("installation clobbered a concurrent disable: %v", err)
	}
	listed, err := catalog.List()
	if err != nil || len(listed) != 1 || !listed[0].Disabled || listed[0].Revision != first.Revision {
		t.Fatalf("concurrent disable was lost: %v, %v", listed, err)
	}
}

func TestCatalogRejectsInvalidPackages(t *testing.T) {
	tests := map[string]func(*sdk.Manifest){
		"traversal":          func(manifest *sdk.Manifest) { manifest.Entry = "../worker" },
		"absolute":           func(manifest *sdk.Manifest) { manifest.Entry = "/bin/sh" },
		"windows-absolute":   func(manifest *sdk.Manifest) { manifest.Entry = `C:\worker.exe` },
		"windows-traversal":  func(manifest *sdk.Manifest) { manifest.Entry = `..\worker.exe` },
		"unclean":            func(manifest *sdk.Manifest) { manifest.Entry = "bin/../worker" },
		"id-traversal":       func(manifest *sdk.Manifest) { manifest.ID = "../outside" },
		"version-traversal":  func(manifest *sdk.Manifest) { manifest.Version = "../outside" },
		"unknown-runtime":    func(manifest *sdk.Manifest) { manifest.Runtime = "sh" },
		"directory-entry":    func(manifest *sdk.Manifest) { manifest.Entry = "bin" },
		"missing-entry":      func(manifest *sdk.Manifest) { manifest.Entry = "absent" },
		"unsupported-schema": func(manifest *sdk.Manifest) { manifest.Schema = 99 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			source := catalogTestPackage(t, "sample", "1.0.0")
			data, err := os.ReadFile(filepath.Join(source, sdk.ManifestFile))
			if err != nil {
				t.Fatal(err)
			}
			var manifest sdk.Manifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			mutate(&manifest)
			catalogTestWriteManifest(t, source, manifest)
			catalog := NewCatalog(t.TempDir())
			checked := false
			if _, err := catalog.Install(t.Context(), source, func(context.Context, Installed) error {
				checked = true
				return nil
			}); err == nil || checked {
				t.Fatalf("invalid package reached handshake: %v, checked=%v", err, checked)
			}
		})
	}
	for _, data := range []string{"{", "null", `{"schema":1,"unknown":true}`} {
		t.Run("manifest-"+data, func(t *testing.T) {
			source := catalogTestPackage(t, "sample", "1.0.0")
			catalogTestWrite(t, filepath.Join(source, sdk.ManifestFile), []byte(data), 0o600)
			if _, err := NewCatalog(t.TempDir()).Install(t.Context(), source, catalogTestCheck); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	t.Run("nonexecutable", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows executability is not represented by Unix mode bits")
		}
		source := catalogTestPackage(t, "sample", "1.0.0")
		if err := os.Chmod(filepath.Join(source, "bin", "worker"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewCatalog(t.TempDir()).Install(t.Context(), source, catalogTestCheck); err == nil {
			t.Fatal("nonexecutable exec entry accepted")
		}
	})
	t.Run("bun", func(t *testing.T) {
		source := catalogTestPackage(t, "sample", "1.0.0")
		catalogTestWriteManifest(t, source, sdk.Manifest{
			Schema: 1, ID: "sample", Version: "1.0.0", Runtime: "bun", Entry: "bin/worker",
			Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1}, Capabilities: []sdk.Capability{sdk.OperationControl}},
		})
		if err := os.Chmod(filepath.Join(source, "bin", "worker"), 0o600); err != nil {
			t.Fatal(err)
		}
		catalogTestInstall(t, NewCatalog(t.TempDir()), source)
	})
	t.Run("recursive-source", func(t *testing.T) {
		source := catalogTestPackage(t, "sample", "1.0.0")
		if _, err := NewCatalog(filepath.Join(source, "catalog")).Install(t.Context(), source, catalogTestCheck); err == nil {
			t.Fatal("catalog inside the source accepted")
		}
	})
}

func TestCatalogRejectsSymlinks(t *testing.T) {
	for _, name := range []string{sdk.ManifestFile, "bin/worker", "bin", ".hidden", "empty-link"} {
		t.Run(name, func(t *testing.T) {
			source := catalogTestPackage(t, "sample", "1.0.0")
			filename := filepath.Join(source, name)
			if err := os.RemoveAll(filename); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filename); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("symlink privilege unavailable: %v", err)
				}
				t.Fatal(err)
			}
			if _, err := NewCatalog(t.TempDir()).Install(t.Context(), source, catalogTestCheck); err == nil {
				t.Fatalf("package symlink %q accepted", name)
			}
		})
	}
	for _, name := range []string{"catalog.json", "catalog.lock", "packages"} {
		t.Run("managed-"+name, func(t *testing.T) {
			catalog := NewCatalog(t.TempDir())
			target := t.TempDir()
			if name != "packages" {
				target = filepath.Join(target, "external.json")
				catalogTestWrite(t, target, []byte("[]"), 0o600)
			}
			if err := os.Symlink(target, filepath.Join(catalog.Root, name)); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("symlink privilege unavailable: %v", err)
				}
				t.Fatal(err)
			}
			if _, err := catalog.Install(t.Context(), catalogTestPackage(t, "sample", "1.0.0"), catalogTestCheck); err == nil {
				t.Fatalf("managed symlink %q accepted", name)
			}
		})
	}
}

func TestCatalogVerifyTampering(t *testing.T) {
	tests := map[string]func(*testing.T, *Catalog, *Installed){
		"contents": func(t *testing.T, catalog *Catalog, entry *Installed) {
			catalogTestWrite(t, filepath.Join(catalog.Root, entry.Directory, "bin", "worker"), []byte("tampered"), 0o700)
		},
		"missing": func(t *testing.T, catalog *Catalog, entry *Installed) {
			if err := os.Remove(filepath.Join(catalog.Root, entry.Directory, ".hidden")); err != nil {
				t.Fatal(err)
			}
		},
		"extra": func(t *testing.T, catalog *Catalog, entry *Installed) {
			catalogTestWrite(t, filepath.Join(catalog.Root, entry.Directory, "surprise"), []byte("extra"), 0o600)
		},
		"manifest": func(t *testing.T, catalog *Catalog, entry *Installed) {
			manifest := entry.Manifest
			manifest.Args = []string{"tampered"}
			catalogTestWriteManifest(t, filepath.Join(catalog.Root, entry.Directory), manifest)
		},
		"frozen-descriptors": func(t *testing.T, catalog *Catalog, entry *Installed) {
			entry.Manifest.Args = []string{"changed catalog"}
			entry.Revision = catalogRevision(*entry)
		},
		"relative-directory": func(t *testing.T, catalog *Catalog, entry *Installed) {
			entry.Directory = "../elsewhere"
			entry.Revision = catalogRevision(*entry)
		},
		"unmanaged-directory": func(t *testing.T, catalog *Catalog, entry *Installed) {
			entry.Directory = "state/sample/1.0.0-test"
			entry.Revision = catalogRevision(*entry)
		},
		"wrong-identity-directory": func(t *testing.T, catalog *Catalog, entry *Installed) {
			entry.Directory = "packages/other/1.0.0-test"
			entry.Revision = catalogRevision(*entry)
		},
		"file-traversal": func(t *testing.T, catalog *Catalog, entry *Installed) {
			entry.Files["../outside"] = strings.Repeat("0", 64)
			entry.Revision = catalogRevision(*entry)
		},
		"invalid-digest": func(t *testing.T, catalog *Catalog, entry *Installed) {
			entry.Files[".hidden"] = "invalid"
			entry.Revision = catalogRevision(*entry)
		},
		"revision": func(t *testing.T, catalog *Catalog, entry *Installed) {
			entry.Revision = strings.Repeat("0", 64)
		},
	}
	for name, tamper := range tests {
		t.Run(name, func(t *testing.T) {
			catalog := NewCatalog(t.TempDir())
			entry := catalogTestInstall(t, catalog, catalogTestPackage(t, "sample", "1.0.0"))
			tamper(t, catalog, &entry)
			if err := catalog.Verify(entry); err == nil {
				t.Fatal("tampered generation verified")
			}
		})
	}
}

func TestCatalogBoundsAndCancellation(t *testing.T) {
	t.Run("file-size", func(t *testing.T) {
		source := catalogTestPackage(t, "sample", "1.0.0")
		file, err := os.Create(filepath.Join(source, "too-large"))
		if err != nil {
			t.Fatal(err)
		}
		err = file.Truncate(catalogFileLimit + 1)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("create sparse oversized fixture: %v, %v", err, closeErr)
		}
		if _, err := NewCatalog(t.TempDir()).Install(t.Context(), source, catalogTestCheck); err == nil {
			t.Fatal("oversized file accepted")
		}
	})
	t.Run("total-size", func(t *testing.T) {
		source := catalogTestPackage(t, "sample", "1.0.0")
		for index := range 5 {
			file, err := os.Create(filepath.Join(source, fmt.Sprintf("large-%d", index)))
			if err != nil {
				t.Fatal(err)
			}
			err = file.Truncate(catalogFileLimit)
			closeErr := file.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("create sparse package fixture: %v, %v", err, closeErr)
			}
		}
		catalog := NewCatalog(t.TempDir())
		checked := false
		if _, err := catalog.Install(t.Context(), source, func(context.Context, Installed) error {
			checked = true
			return nil
		}); err == nil || checked {
			t.Fatalf("total package bound did not guard the handshake: %v, checked=%v", err, checked)
		}
		if _, err := os.Stat(filepath.Join(catalog.Root, "catalog.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("oversized package published a catalog: %v", err)
		}
	})
	t.Run("file-count", func(t *testing.T) {
		source := catalogTestPackage(t, "sample", "1.0.0")
		for index := range catalogFilesLimit {
			name := filepath.Join(source, "many", fmt.Sprintf("file-%04d", index))
			catalogTestWrite(t, name, nil, 0o600)
		}
		if _, err := NewCatalog(t.TempDir()).Install(t.Context(), source, catalogTestCheck); err == nil {
			t.Fatal("package with too many files accepted")
		}
	})
	t.Run("cancel-before", func(t *testing.T) {
		catalog := NewCatalog(filepath.Join(t.TempDir(), "absent"))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := catalog.Install(ctx, catalogTestPackage(t, "sample", "1.0.0"), catalogTestCheck); !errors.Is(err, context.Canceled) {
			t.Fatalf("install ignored cancellation: %v", err)
		}
		if err := catalog.SetEnabled(ctx, "sample", true); !errors.Is(err, context.Canceled) {
			t.Fatalf("enable ignored cancellation: %v", err)
		}
		if err := catalog.Remove(ctx, "sample"); !errors.Is(err, context.Canceled) {
			t.Fatalf("remove ignored cancellation: %v", err)
		}
	})
	t.Run("cancel-handshake", func(t *testing.T) {
		catalog := NewCatalog(t.TempDir())
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var directory string
		_, err := catalog.Install(ctx, catalogTestPackage(t, "sample", "1.0.0"), func(ctx context.Context, entry Installed) error {
			directory = entry.Directory
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("published after handshake canceled: %v", err)
		}
		if _, err := os.Stat(filepath.Join(catalog.Root, directory)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("canceled candidate retained: %v", err)
		}
	})
	t.Run("cancel-lock", func(t *testing.T) {
		catalog := NewCatalog(t.TempDir())
		entry := catalogTestInstall(t, catalog, catalogTestPackage(t, "sample", "1.0.0"))
		lock, err := os.OpenFile(filepath.Join(catalog.Root, "catalog.lock"), os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if held, err := filelock.TryLock(lock); err != nil || !held {
			t.Fatalf("hold lock: held=%v, err=%v", held, err)
		}
		defer filelock.Unlock(lock)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := catalog.SetEnabled(ctx, entry.Manifest.ID, false); !errors.Is(err, context.Canceled) {
			t.Fatalf("locked operation ignored cancellation: %v", err)
		}
	})
}
