package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

func holdPrivateStoreTestLock(t *testing.T, store *Store) func() {
	t.Helper()
	if err := store.withPrivateStore(t.Context(), func(*os.Root) error { return nil }); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(store.root, "private", ".lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	held, err := filelock.TryLock(lock)
	if err != nil || !held {
		lock.Close()
		t.Fatalf("hold private store lock: %v, %v", held, err)
	}
	return func() {
		filelock.Unlock(lock)
		lock.Close()
	}
}

func TestWindowMetadataAndConfigRespectDeadline(t *testing.T) {
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { managerReleaseClose(t, manager) })
	entry := catalogTestInstall(t, manager.catalog, moduleWindowPackage(t, "1.0.0"))
	revision := windowRevision(entry.Revision, "0")
	catalog, err := os.OpenFile(filepath.Join(manager.catalog.Root, "catalog.lock"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	for _, barrier := range []string{"catalog", "configuration"} {
		t.Run(barrier, func(t *testing.T) {
			for _, operation := range []struct {
				name string
				run  func(context.Context) error
			}{
				{"list", func(ctx context.Context) error {
					_, err := manager.Windows(ctx)
					return err
				}},
				{"metadata", func(ctx context.Context) error {
					_, err := manager.WindowsWithMetadata(ctx)
					return err
				}},
				{"asset", func(ctx context.Context) error {
					_, err := manager.ReadWindowFile(ctx, entry.Manifest.ID, revision, "ui/index.html")
					return err
				}},
				{"configure", func(ctx context.Context) error {
					_, err := manager.CompareAndSwapWindowConfig(ctx, entry.Manifest.ID, revision, "0", json.RawMessage(`{"changed":true}`))
					return err
				}},
			} {
				t.Run(operation.name, func(t *testing.T) {
					var unlock func()
					if barrier == "catalog" {
						held, err := filelock.TryLock(catalog)
						if err != nil || !held {
							t.Fatalf("hold catalog lock: %t, %v", held, err)
						}
						unlock = func() { filelock.Unlock(catalog) }
					} else {
						unlock = holdPrivateStoreTestLock(t, manager.store)
					}
					ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
					defer cancel()
					done := make(chan error, 1)
					go func() { done <- operation.run(ctx) }()
					select {
					case err := <-done:
						unlock()
						if !errors.Is(err, context.DeadlineExceeded) {
							t.Fatalf("window %s returned %v, want deadline", operation.name, err)
						}
					case <-time.After(1200 * time.Millisecond):
						unlock()
						<-done
						t.Fatalf("window %s waited past its deadline for %s", operation.name, barrier)
					}
				})
			}
		})
	}
	modules, err := manager.Windows(t.Context())
	if err != nil || len(modules) != 1 || modules[0].Revision != revision {
		t.Fatalf("configuration changed during the canceled requests: %+v, %v", modules, err)
	}
	body, err := manager.ReadWindowFile(t.Context(), entry.Manifest.ID, revision, "ui/index.html")
	if err != nil || string(body) != "<p>module page</p>" {
		t.Fatalf("frozen window asset after releasing locks: %q, %v", body, err)
	}
}
