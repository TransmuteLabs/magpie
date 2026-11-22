package extension

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func closeActivationTestManager(t *testing.T, manager *Manager) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
}

func waitActivation(t *testing.T, want string, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("activation did not reach %s", want)
		}
	}
}

func TestGatewayActivationDoesNotPollCatalogAtIdle(t *testing.T) {
	manager := NewManager(t.TempDir())
	closeActivationTestManager(t, manager)
	if err := os.WriteFile(filepath.Join(manager.root, "catalog.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(manager.root, "catalog.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	take := func() {
		t.Helper()
		waitActivation(t, "fixture catalog lock", func() bool {
			locked, err := filelock.TryLock(lock)
			if err != nil {
				t.Fatal(err)
			}
			return locked
		})
	}
	readerIdle := func() bool {
		if !manager.reloadMu.TryLock() {
			return false
		}
		manager.reloadMu.Unlock()
		return true
	}
	take()
	owner, err := manager.BeginGatewayActivation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	waitActivation(t, "blocked initial catalog read", func() bool { return !readerIdle() })
	filelock.Unlock(lock)
	waitActivation(t, "initial catalog read completed", readerIdle)
	take()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !readerIdle() {
			t.Fatal("activation attempted another catalog read while idle")
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := owner.Close(ctx); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
}

func TestGatewayActivationFollowsExternalPublication(t *testing.T) {
	const childRoot = "MAGPIE_TEST_ACTIVATION_ROOT"
	const childOperation = "MAGPIE_TEST_ACTIVATION_OPERATION"
	const childSource = "MAGPIE_TEST_ACTIVATION_SOURCE"
	const id = "memory-fixture"
	if root := os.Getenv(childRoot); root != "" {
		manager := NewManager(root)
		var err error
		switch os.Getenv(childOperation) {
		case "install":
			_, err = manager.Install(t.Context(), os.Getenv(childSource))
		case "configure":
			_, err = manager.CompareAndSwapModuleConfig(t.Context(), id, "0", json.RawMessage(`{"external":true}`))
		case "disable":
			err = manager.SetEnabled(t.Context(), id, false)
		case "enable":
			err = manager.SetEnabled(t.Context(), id, true)
		default:
			t.Fatal("unknown external publication fixture operation")
		}
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	source := gatewayFixturePackage(t, "1.0.0", sdk.GatewayHookDescriptor{Request: true})
	manifestPath := filepath.Join(source, sdk.ManifestFile)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest sdk.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Contract.Version.Minor = 8
	manifest.Activation = []string{sdk.ActivationGateway}
	catalogTestWriteManifest(t, source, manifest)
	root := filepath.Join(t.TempDir(), "config", "extensions")
	manager := NewManager(root)
	closeActivationTestManager(t, manager)
	if _, err := manager.BeginGatewayActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	publish := func(operation string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		command := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestGatewayActivationFollowsExternalPublication$")
		command.Env = append(os.Environ(), childRoot+"="+root, childOperation+"="+operation, childSource+"="+source)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("external %s: %v\n%s", operation, err, output)
		}
	}
	running := func(version string) bool {
		manager.mu.Lock()
		worker := manager.workers[id]
		manager.mu.Unlock()
		return worker != nil && worker.config.Version == version && manager.Running(id)
	}
	publish("install")
	waitActivation(t, "external install", func() bool { return running("0") })
	publish("configure")
	waitActivation(t, "external config replacement", func() bool { return running("1") })
	publish("disable")
	waitActivation(t, "external disable", func() bool { return !manager.Running(id) })
	publish("enable")
	waitActivation(t, "external re-enable", func() bool { return running("1") })

	catalog := filepath.Join(root, "catalog.json")
	raw, err = os.ReadFile(catalog)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(catalog)
	if err != nil {
		t.Fatal(err)
	}
	replace := func(data []byte) {
		t.Helper()
		temp := filepath.Join(root, ".test-replace")
		if err := os.WriteFile(temp, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(temp, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temp, catalog); err != nil {
			t.Fatal(err)
		}
	}
	replace(bytes.Repeat([]byte("["), len(raw)))
	if lease, err := manager.Acquire(t.Context(), id); err == nil {
		lease.Close()
		t.Fatal("strict admission accepted a corrupt replacement")
	}
	waitActivation(t, "withdrawal after catalog corruption", func() bool { return !manager.Running(id) })
	replace(raw)
	waitActivation(t, "restart after catalog repair", func() bool { return running("1") })
	currentWorker := func() *managedWorker {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return manager.workers[id]
	}
	worker := currentWorker()
	worker.mu.Lock()
	client := worker.client
	worker.mu.Unlock()
	client.fail(errors.New("fixture interrupted activation transport"))
	waitActivation(t, "restart after transport failure", func() bool {
		return currentWorker() != worker && running("1")
	})
}

func TestGatewayActivationFallsBackAfterWatchFailure(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(root)
	closeActivationTestManager(t, manager)
	path := filepath.Join(root, "catalog.json")
	if err := os.WriteFile(path, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	startedWatch, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = startedWatch.Close() })
	var attempts atomic.Int32
	var publication atomic.Bool
	replacement := []byte("[]\n")
	_, err = manager.beginGatewayActivation(t.Context(), func() (*fsnotify.Watcher, error) {
		if attempts.Add(1) == 1 {
			return startedWatch, nil
		}
		publication.Store(os.WriteFile(path, replacement, 0o600) == nil)
		return nil, errors.New("fixture watch unavailable")
	})
	if err != nil {
		t.Fatal(err)
	}
	waitActivation(t, "initial catalog read with a live watch", func() bool {
		return manager.catalog.listSnapshot.Load() != nil
	})
	if err := startedWatch.Close(); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(replacement)
	waitActivation(t, "fallback reconciliation with an unavailable watch", func() bool {
		current := manager.catalog.listSnapshot.Load()
		return publication.Load() && current != nil && current.digest == want
	})
}

func TestActivationWatchFollowsAtomicReplacement(t *testing.T) {
	for _, mode := range []struct {
		name      string
		filesOnly bool
	}{{"directory", false}, {"file", true}} {
		t.Run(mode.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "config", "extensions")
			path := filepath.Join(root, "catalog.json")
			watch := newActivationWatch(root)
			if !mode.filesOnly && watch.filesOnly {
				t.Skip("directory watching is not used on kqueue")
			}
			watch.filesOnly = mode.filesOnly
			watch.open(fsnotify.NewWatcher)
			defer watch.close()
			if watch.backend == nil {
				t.Fatal("fixture watcher unavailable")
			}
			waitEvent := func(path string) {
				t.Helper()
				deadline := time.NewTimer(10 * time.Second)
				defer deadline.Stop()
				for {
					select {
					case event, open := <-watch.events():
						if !open {
							t.Fatal("fixture watcher closed")
						}
						if filepath.Clean(event.Name) == path && watch.relevant(event) {
							return
						}
					case err := <-watch.errors():
						t.Fatalf("watch error: %v", err)
					case <-deadline.C:
						t.Fatalf("did not see publication of %s", path)
					}
				}
			}
			publish := func(path string) {
				t.Helper()
				temp := filepath.Join(filepath.Dir(path), ".publish")
				if err := os.WriteFile(temp, []byte("[]"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(temp, path); err != nil {
					t.Fatal(err)
				}
			}
			watch.begin()
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if !mode.filesOnly {
				waitEvent(filepath.Dir(root))
			}
			publish(path)
			watch.begin()
			watch.finish()
			publish(path)
			waitEvent(path)
			watch.begin()
			watch.finish()
			if mode.filesOnly {
				if len(watch.watched) != 1 || watch.watched[path] == nil {
					t.Fatal("file watcher opened paths outside the selected file")
				}
			} else {
				old := filepath.Join(filepath.Dir(root), "old")
				if err := os.Rename(root, old); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				waitEvent(root)
				watch.begin()
				watch.finish()
			}
			publish(path)
			waitEvent(path)
			configDir := filepath.Join(root, "private", "fixture", "state")
			watch.begin()
			watch.followConfig("fixture")
			watch.finish()
			if err := os.MkdirAll(configDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if !mode.filesOnly {
				waitEvent(filepath.Join(root, "private"))
			}
			config := filepath.Join(configDir, storeStateName(sdk.Scope{Plugin: "fixture"}, moduleConfigKey))
			publish(config)
			watch.begin()
			watch.followConfig("fixture")
			watch.finish()
			publish(config)
			waitEvent(config)
		})
	}
}
