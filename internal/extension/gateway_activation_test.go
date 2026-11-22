package extension

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
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

func activationFixturePackage(t *testing.T) string {
	t.Helper()
	source := gatewayFixturePackage(t, "1.0.0", sdk.GatewayHookDescriptor{Request: true})
	raw, err := os.ReadFile(filepath.Join(source, sdk.ManifestFile))
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
	return source
}

func parkActivationProgram(t *testing.T, manager *Manager, entry Installed) (restore func() error) {
	t.Helper()
	program := filepath.Join(manager.root, entry.Directory, entry.Manifest.Entry)
	parked := filepath.Join(t.TempDir(), "worker")
	if err := os.Rename(program, parked); err != nil {
		t.Fatal(err)
	}
	return func() error { return os.Rename(parked, program) }
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
	source := activationFixturePackage(t)
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
	raw, err := os.ReadFile(catalog)
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

func TestGatewayActivationWakesWhenCatalogAppears(t *testing.T) {
	root := filepath.Join(t.TempDir(), "extensions")
	manager := NewManager(root)
	closeActivationTestManager(t, manager)
	entry, err := manager.Install(t.Context(), activationFixturePackage(t))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "catalog.json")
	parked := path + ".parked"
	if _, err := manager.BeginGatewayActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitActivation(t, "initial activation", func() bool { return manager.Running(entry.Manifest.ID) })
	if err := os.Rename(path, parked); err != nil {
		t.Fatal(err)
	}
	waitActivation(t, "withdrawal while catalog is absent", func() bool { return !manager.Running(entry.Manifest.ID) })
	if err := os.Rename(parked, path); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(gatewayActivationFallback / 2)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !manager.Running(entry.Manifest.ID) {
		select {
		case <-deadline.C:
			t.Fatal("catalog publication did not activate the worker before the fallback")
		case <-tick.C:
		}
	}
}

func TestActivationWatchWakesWhenRootAppears(t *testing.T) {
	root := filepath.Join(t.TempDir(), "outer", "extensions")
	watch := newActivationWatch(root)
	watch.open(nil)
	defer watch.close()
	if !watch.active() {
		t.Fatal("fixture watcher unavailable")
	}
	watch.begin()
	watch.finish()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "catalog.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(gatewayActivationFallback / 2)
	defer deadline.Stop()
	for {
		select {
		case event, open := <-watch.events():
			if !open {
				t.Fatal("fixture watcher closed")
			}
			if watch.relevant(event) {
				return
			}
		case err := <-watch.errors():
			t.Fatalf("watch error: %v", err)
		case _, open := <-watch.vnodeChanges():
			if !open {
				t.Fatal("fixture watcher closed")
			}
			if watch.vnodeRelevant() {
				return
			}
		case <-deadline.C:
			t.Fatal("publication under a previously absent root did not schedule reconciliation")
		}
	}
}

func TestGatewayActivationRetriesUnchangedGeneration(t *testing.T) {
	manager := NewManager(t.TempDir())
	closeActivationTestManager(t, manager)
	entry, err := manager.Install(t.Context(), activationFixturePackage(t))
	if err != nil {
		t.Fatal(err)
	}
	restoreProgram := parkActivationProgram(t, manager, entry)
	id := entry.Manifest.ID

	repaired := make(chan error, 1)
	wakeups := 0
	_, err = manager.beginGatewayActivation(t.Context(), func() (*fsnotify.Watcher, error) {
		wakeups++
		if wakeups == 2 {
			manager.mu.Lock()
			selected := manager.workers[id] != nil
			manager.mu.Unlock()
			if !selected || manager.Running(id) {
				repaired <- errors.New("activation did not fail after selecting the installed module")
			} else {
				repaired <- restoreProgram()
			}
		}
		return nil, errors.New("fixture watch unavailable")
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-repaired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("activation did not complete the first failed start")
	}

	waitActivation(t, "start after a transient failure at the same generation", func() bool {
		return manager.Running(id)
	})
	entries, err := manager.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Revision != entry.Revision || entries[0].EnableGeneration != entry.EnableGeneration {
		t.Fatal("fixture repair changed the installed generation")
	}
}

func TestGatewayActivationBackoffIsScopedToGeneration(t *testing.T) {
	manager := NewManager(t.TempDir())
	closeActivationTestManager(t, manager)
	entry, err := manager.Install(t.Context(), activationFixturePackage(t))
	if err != nil {
		t.Fatal(err)
	}
	restoreProgram := parkActivationProgram(t, manager, entry)
	id := entry.Manifest.ID
	owner := activationTestOwner(t, manager)
	watch := newActivationWatch(manager.root)

	for _, seconds := range []int{1, 2, 4, 8, 16, 32, 60, 60} {
		before := time.Now()
		reconcileActivationTest(t, owner, watch)
		retry := owner.retries[id]
		want := time.Duration(seconds) * time.Second
		if retry.delay != want || retry.next.Before(before.Add(want)) {
			t.Fatalf("failed start scheduled %v at %v, want %v backoff", retry.delay, retry.next, want)
		}
		retry.next = time.Time{}
		owner.retries[id] = retry
	}
	retry := owner.retries[id]
	retry.next = time.Now().Add(time.Hour)
	owner.retries[id] = retry
	if err := restoreProgram(); err != nil {
		t.Fatal(err)
	}
	reconcileActivationTest(t, owner, watch)
	if manager.Running(id) || owner.retries[id] != retry {
		t.Fatal("an early reconciliation ignored the generation's backoff")
	}
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), id, "0", json.RawMessage(`{"fixture":true}`)); err != nil {
		t.Fatal(err)
	}
	reconcileActivationTest(t, owner, watch)
	lease := owner.leases[id]
	if !manager.Running(id) || lease.Lease == nil || lease.worker.config.Version != "1" || len(owner.retries) != 0 {
		t.Fatal("a new configuration generation inherited the old backoff")
	}
}

func TestGatewayActivationBacksOffAfterWorkerExit(t *testing.T) {
	manager := NewManager(t.TempDir())
	closeActivationTestManager(t, manager)
	entry, err := manager.Install(t.Context(), activationFixturePackage(t))
	if err != nil {
		t.Fatal(err)
	}
	id := entry.Manifest.ID
	owner := activationTestOwner(t, manager)
	watch := newActivationWatch(manager.root)
	reconcileActivationTest(t, owner, watch)
	delays := []int{1, 2, 4, 8, 16, 32, 60, 60}
	for attempt, seconds := range delays {
		lease := owner.leases[id]
		if lease.Lease == nil || !manager.Running(id) {
			t.Fatal("fixture did not retain a live worker")
		}
		stopActivationWorker(t, lease.client)
		select {
		case <-owner.changed:
		case <-time.After(10 * time.Second):
			t.Fatal("worker exit did not schedule reconciliation")
		}
		before := time.Now()
		reconcileActivationTest(t, owner, watch)
		if manager.Running(id) || owner.leases[id].Lease != nil {
			t.Fatal("activation immediately replaced its failed worker")
		}
		retry := owner.retries[id]
		want := time.Duration(seconds) * time.Second
		if retry.delay != want || retry.next.Before(before.Add(want)) {
			t.Fatalf("worker exit scheduled %v at %v, want %v backoff", retry.delay, retry.next, want)
		}
		waitActivationComponent(t, "original group retirement", lease.retired)
		if attempt+1 != len(delays) {
			retry.next = time.Time{}
			owner.retries[id] = retry
			reconcileActivationTest(t, owner, watch)
		}
	}
	retry := owner.retries[id]
	retry.next = time.Now().Add(time.Hour)
	owner.retries[id] = retry
	reconcileActivationTest(t, owner, watch)
	if manager.Running(id) || owner.retries[id] != retry {
		t.Fatal("reconciliation bypassed the backoff or counted the same exit twice")
	}
	if err := manager.SetEnabled(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetEnabled(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	reconcileActivationTest(t, owner, watch)
	renewed := owner.leases[id]
	if renewed.Lease == nil || !manager.Running(id) ||
		renewed.worker.entry.EnableGeneration == entry.EnableGeneration || len(owner.retries) != 0 {
		t.Fatal("new enablement epoch did not start immediately")
	}
}

func TestGatewayActivationRejectsWorkerExitedBeforeRetention(t *testing.T) {
	manager := NewManager(t.TempDir())
	closeActivationTestManager(t, manager)
	entry, err := manager.Install(t.Context(), activationFixturePackage(t))
	if err != nil {
		t.Fatal(err)
	}
	id := entry.Manifest.ID
	owner := activationTestOwner(t, manager)
	worker, client, err := manager.acquire(t.Context(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	stopActivationWorker(t, client)
	if err := manager.checkActiveGeneration(t.Context(), worker.entry, worker.config.Version); err != nil {
		releaseWorker(worker)
		t.Fatalf("fixture generation changed: %v", err)
	}
	generation := moduleGeneration{worker.entry, worker.config}
	owner.desired = map[string]moduleGeneration{id: generation}
	owner.starting[id] = activationStart{generation: generation}
	owner.finishStart(activationResult{id: id, lease: manager.newLease(owner.ctx, worker, client)})
	if owner.leases[id].Lease != nil || owner.retries[id].delay != gatewayActivationRetryStart {
		t.Fatal("dead worker passed activation admission or failed to schedule a retry")
	}
}

func TestGatewayActivationResetsBackoffAfterStableWorker(t *testing.T) {
	manager := NewManager(t.TempDir())
	closeActivationTestManager(t, manager)
	entry, err := manager.Install(t.Context(), activationFixturePackage(t))
	if err != nil {
		t.Fatal(err)
	}
	id := entry.Manifest.ID
	owner := activationTestOwner(t, manager)
	watch := newActivationWatch(manager.root)
	owner.retries[id] = activationRetry{
		identity: activationIdentity(entry, "0"),
		delay:    32 * time.Second,
	}
	reconcileActivationTest(t, owner, watch)
	lease := owner.leases[id]
	if lease.Lease == nil || owner.retries[id].delay != 32*time.Second {
		t.Fatal("handshake cleared the existing failure streak")
	}
	lease.since = time.Now().Add(-2 * gatewayActivationStable)
	owner.leases[id] = lease
	reconcileActivationTest(t, owner, watch)
	if !manager.Running(id) || len(owner.retries) != 0 {
		t.Fatal("stable worker did not clear the existing failure streak")
	}
	stopActivationWorker(t, lease.client)
	reconcileActivationTest(t, owner, watch)
	if manager.Running(id) || owner.retries[id].delay != gatewayActivationRetryStart {
		t.Fatal("worker exit after stability retained the previous failure streak")
	}
}

func activationTestOwner(t *testing.T, manager *Manager) *GatewayActivation {
	t.Helper()
	owner := newGatewayActivation(manager, t.Context())
	t.Cleanup(owner.shutdown)
	return owner
}

func reconcileActivationTest(t *testing.T, owner *GatewayActivation, watch *activationWatch) {
	t.Helper()
	owner.reconcile(watch)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	for len(owner.starting) != 0 {
		select {
		case result := <-owner.started:
			owner.finishStart(result)
			owner.startReady()
		case <-ctx.Done():
			t.Fatal("activation did not finish its startup attempts")
		}
	}
}

func TestGatewayActivationLimitsAndJoinsPendingStarts(t *testing.T) {
	manager := NewManager(t.TempDir())
	closeActivationTestManager(t, manager)
	manager.reloadMu.Lock()
	defer manager.reloadMu.Unlock()
	owner := newGatewayActivation(manager, t.Context())
	defer owner.shutdown()
	owner.desired = make(map[string]moduleGeneration)
	for index := range gatewayActivationStarts * 2 {
		id := "activation-pending-" + strconv.Itoa(index)
		owner.desired[id] = moduleGeneration{
			entry: Installed{Manifest: sdk.Manifest{
				ID: id, Activation: []string{sdk.ActivationGateway},
				Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8}},
			}},
		}
	}
	owner.startReady()
	owner.startReady()
	if got := len(owner.starting); got != gatewayActivationStarts {
		t.Fatalf("queued %d concurrent starts, want %d", got, gatewayActivationStarts)
	}
	owner.shutdown()
	if len(owner.starting) != 0 || len(owner.started) != 0 || len(owner.leases) != 0 {
		t.Fatal("canceled activation left an unjoined start")
	}
}

func stopActivationWorker(t *testing.T, client *workerClient) {
	t.Helper()
	client.fail(errors.New("fixture interrupted activation transport"))
	select {
	case <-client.done:
	case <-time.After(10 * time.Second):
		t.Fatal("fixture worker did not exit")
	}
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

func TestActivationWatchRearmsAfterRenameBack(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("fixture uses inotify or kqueue self-rename events")
	}
	for _, mode := range []struct {
		name      string
		filesOnly bool
	}{{"directory", false}, {"file", true}} {
		t.Run(mode.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "extensions")
			watch := newActivationWatch(root)
			if !mode.filesOnly && watch.filesOnly {
				t.Skip("directory watching is not used on kqueue")
			}
			watch.filesOnly = mode.filesOnly
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			catalog := filepath.Join(root, "catalog.json")
			if err := os.WriteFile(catalog, []byte("[]"), 0o600); err != nil {
				t.Fatal(err)
			}
			target := root
			if mode.filesOnly {
				target = catalog
			}
			before, err := os.Lstat(target)
			if err != nil {
				t.Fatal(err)
			}
			watch.open(fsnotify.NewWatcher)
			defer watch.close()
			if watch.backend == nil {
				t.Fatal("fixture watcher unavailable")
			}
			watch.begin()
			watch.finish()
			if !slices.Contains(watch.backend.WatchList(), target) {
				t.Fatal("fixture path is not registered")
			}
			wait := func(description string, ready func(fsnotify.Event) bool) {
				t.Helper()
				timer := time.NewTimer(5 * time.Second)
				defer timer.Stop()
				for {
					select {
					case event, open := <-watch.events():
						if !open {
							t.Fatal("fixture watcher closed")
						}
						if ready(event) {
							return
						}
					case err := <-watch.errors():
						t.Fatalf("watch error: %v", err)
					case <-timer.C:
						t.Fatalf("watch did not see %s", description)
					}
				}
			}
			parked := target + ".parked"
			if err := os.Rename(target, parked); err != nil {
				t.Fatal(err)
			}
			wait("backend remove the renamed registration", func(event fsnotify.Event) bool {
				return filepath.Clean(event.Name) == target && event.Has(fsnotify.Rename) &&
					!slices.Contains(watch.backend.WatchList(), target) && watch.relevant(event)
			})
			if err := os.Rename(parked, target); err != nil {
				t.Fatal(err)
			}
			if after, err := os.Lstat(target); err != nil || !os.SameFile(before, after) {
				t.Fatalf("rename-back changed the fixture inode: %v", err)
			}
			watch.begin()
			watch.finish()
			if err := os.WriteFile(catalog, []byte("[]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			wait("a catalog write after rename-back", func(event fsnotify.Event) bool {
				return filepath.Clean(event.Name) == catalog && event.Has(fsnotify.Write) && watch.relevant(event)
			})
		})
	}
}
