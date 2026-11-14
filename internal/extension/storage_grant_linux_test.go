package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func storageGrantFixture(t *testing.T) (*StorageGrant, context.Context, string) {
	t.Helper()
	manager := NewManager(t.TempDir())
	entry := installComponentFixture(t, manager, componentFixtureManifest())
	entry.Manifest.Runtime = "native"
	entry.Manifest.Contract = sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8}, Capabilities: []sdk.Capability{sdk.StorageSource}}
	entry.Manifest.StorageSources = []sdk.StorageSourceDescriptor{{ID: "history", Agent: "fixture", Codec: "fixture-db",
		Roots: []sdk.SessionSourceStore{{Slot: "data", HomeRelativePath: ".fixture/data"}},
		Families: []sdk.StorageFamily{{ID: "history", Primary: "fixture.db", Sidecars: []string{"fixture.db-wal", "fixture.db-shm"},
			Trees: []string{"legacy"}}}, Transcript: true, Resume: "none"}}
	lease := &Lease{manager: manager, worker: &managedWorker{entry: entry, config: sdk.ConfigSnapshot{Version: "0"}},
		client: &workerClient{}, ctx: t.Context()}
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "legacy"), 0o700); err != nil {
		t.Fatal(err)
	}
	grant, err := NewStorageGrant(t.Context(), lease, "history", "history", "root", directory, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { grant.Close() })
	return grant, grant.Context(t.Context()), directory
}

func TestStorageRawSnapshotChargesWholeFileOnce(t *testing.T) {
	grant, _, directory := storageGrantFixture(t)
	body := strings.Repeat("x", 1024)
	if err := os.WriteFile(filepath.Join(directory, "legacy", "session.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	grant.request.Limits.MaxReadBytes = int64(len(body))
	page, err := grant.list(sdk.StorageFilesRequest{Directory: "legacy", Limit: 256})
	if err != nil || len(page.Entries) != 1 {
		t.Fatal(page, err)
	}
	request := sdk.StorageBytesRequest{FileRef: page.Entries[0].FileRef, Offset: 0, Length: 16}
	for range 3 {
		chunk, err := grant.bytes(request)
		if err != nil || string(chunk.Bytes) != body[:16] {
			t.Fatal("cached bounded snapshot could not be read", chunk, err)
		}
	}
	if grant.read != int64(len(body)) {
		t.Fatal("raw snapshot charged returned chunks instead of the whole physical read", grant.read)
	}
}

func TestStorageDirectoryChangeInvalidatesPublication(t *testing.T) {
	for _, mutation := range []string{"new-member", "replaced-directory", "symlink-member"} {
		t.Run(mutation, func(t *testing.T) {
			grant, ctx, directory := storageGrantFixture(t)
			if _, err := grant.list(sdk.StorageFilesRequest{Directory: "legacy", Limit: 256}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "legacy")
			switch mutation {
			case "new-member":
				if err := os.WriteFile(filepath.Join(path, "new.json"), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "replaced-directory":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink-member":
				if err := os.Symlink(t.TempDir(), filepath.Join(path, "foreign")); err != nil {
					t.Fatal(err)
				}
			}
			if grant.Recheck(ctx) == nil {
				t.Fatal("changed directory membership crossed storage publication", mutation)
			}
		})
	}
}

func TestStorageParentAndPublicationCannotChangeSelectedSession(t *testing.T) {
	grant, ctx, _ := storageGrantFixture(t)
	request := sdk.StorageRequest{Lease: grant.RequestLease(), Mode: "transcript",
		Locator: &sdk.StorageSessionLocator{RootRef: "root", SessionID: "selected", Key: "selected"}}
	scope := sdk.Scope{Session: "selected"}
	if _, _, err := beginStorageCall(context.Background(), grant.lease, sdk.MethodStorageRead, scope, request); err == nil {
		t.Fatal("unowned call acquired a native storage descriptor")
	}
	parent, finish, err := beginStorageCall(ctx, grant.lease, sdk.MethodStorageRead, scope, request)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	if _, _, err := beginStorageCall(ctx, grant.lease, sdk.MethodStorageRead, scope, request); err == nil {
		t.Fatal("concurrent parent borrowed the same grant")
	}
	raw, err := json.Marshal(sdk.StorageResult{SnapshotRef: request.Lease.SnapshotRef, Mode: "transcript",
		SessionID: "foreign", Parts: []sdk.SessionSourcePart{}})
	if err != nil {
		t.Fatal(err)
	}
	if finishStorageCall(parent, raw) == nil {
		t.Fatal("foreign session crossed host result publication")
	}
	authority := hostAuthority{manifest: grant.lease.Manifest(), method: sdk.MethodStorageRead}
	lease, _ := json.Marshal(request.Lease)
	access, err := grant.lease.manager.storageHostCall(parent, scope, sdk.MethodStorageNative, lease, authority)
	if err != nil || len(access.(sdk.StorageNativeAccess).Family.Trees) != 0 {
		t.Fatal("selected native parent lost its finite database family", access, err)
	}
	for _, foreign := range []sdk.Scope{{Session: "foreign"}, {Session: "selected", Account: "another"}} {
		if _, err := grant.lease.manager.storageHostCall(parent, foreign, sdk.MethodStorageNative, lease, authority); err == nil {
			t.Fatal("foreign callback borrowed a selected native parent", foreign)
		}
	}
	authority.method = sdk.MethodModuleCall
	if _, err := grant.lease.manager.storageHostCall(parent, sdk.Scope{}, sdk.MethodStorageNative, lease, authority); err == nil {
		t.Fatal("private component borrowed the native descriptor")
	}
	grant.check = func(context.Context) error { return errors.New("consent revoked") }
	if grant.Recheck(parent) == nil {
		t.Fatal("revoked consent remained publishable")
	}
}

func TestStoragePublicationRechecksRootConfigConsentAndLifecycle(t *testing.T) {
	for _, mutation := range []string{"consent", "cancel", "root", "config", "off", "worker", "lease"} {
		t.Run(mutation, func(t *testing.T) {
			grant, ctx, directory := storageGrantFixture(t)
			if err := grant.Recheck(ctx); err != nil {
				t.Fatal("initial storage ownership was not valid", err)
			}
			switch mutation {
			case "consent":
				grant.check = func(context.Context) error { return errors.New("revoked") }
			case "cancel":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			case "root":
				if err := os.Rename(directory, directory+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			case "config":
				if _, err := grant.lease.manager.CompareAndSwapModuleConfig(ctx, grant.lease.worker.entry.Manifest.ID, "0", json.RawMessage(`{"changed":true}`)); err != nil {
					t.Fatal(err)
				}
			case "off":
				if err := grant.lease.manager.catalog.SetEnabled(ctx, grant.lease.worker.entry.Manifest.ID, false); err != nil {
					t.Fatal(err)
				}
			case "worker":
				grant.lease.client.err = errors.New("worker closed")
			case "lease":
				grant.lease.closed.Store(true)
			}
			if grant.Recheck(ctx) == nil {
				t.Fatal("changed storage authority remained publishable", mutation)
			}
		})
	}
}

func TestStorageRawSnapshotCannotHideRewriteBehindRestoredMetadata(t *testing.T) {
	grant, ctx, directory := storageGrantFixture(t)
	path := filepath.Join(directory, "legacy", "session.json")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := grant.list(sdk.StorageFilesRequest{Directory: "legacy", Limit: 256})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := grant.bytes(sdk.StorageBytesRequest{FileRef: page.Entries[0].FileRef, Length: 100}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("change"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if grant.Recheck(ctx) == nil {
		t.Fatal("raw file rewrite with restored metadata crossed publication")
	}
}
