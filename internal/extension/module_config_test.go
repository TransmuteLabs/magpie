package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func installComponentFixture(t *testing.T, manager *Manager, manifest sdk.Manifest) Installed {
	t.Helper()
	source := t.TempDir()
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{sdk.ManifestFile: body, manifest.Entry: []byte("unexecuted primary fixture")}
	for _, service := range manifest.Services {
		files[service.Entry] = []byte("unexecuted service fixture")
	}
	if manifest.Window != nil {
		files[manifest.Window.Metadata] = []byte(`{"name":"composite-fixture-window","magpie":{"page":{"title":"Fixture page","file":"ui/page.html"},"panels":[{"slot":"agents","title":"Fixture panel","file":"ui/panel.html"}]}}`)
		files["ui/page.html"] = []byte(`<!doctype html><html><body>Fixture page</body></html>`)
		files["ui/panel.html"] = []byte(`<!doctype html><html><body>Fixture panel</body></html>`)
	}
	for name, data := range files {
		target := filepath.Join(source, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	entry, err := manager.catalog.Install(t.Context(), source, func(context.Context, Installed) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestModuleConfigCASAndModuleIsolation(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := componentFixtureManifest()
	entry := installComponentFixture(t, manager, manifest)
	other := manifest.Clone()
	other.ID = "other-module"
	installComponentFixture(t, manager, other)
	original, err := manager.ModuleConfig(t.Context(), manifest.ID)
	if err != nil || original.Version != "0" || string(original.Values) != "{}" {
		t.Fatalf("initial module configuration: %+v, %v", original, err)
	}
	values := json.RawMessage(`{"label":"old","nested":{"enabled":true}}`)
	first, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, original.Version, values)
	if err != nil || first.Version != "1" {
		t.Fatalf("first module configuration: %+v, %v", first, err)
	}
	values[2] = 'X'
	first.Values[2] = 'X'
	persisted, err := NewManager(manager.root).ModuleConfig(t.Context(), manifest.ID)
	if err != nil || string(persisted.Values) != `{"label":"old","nested":{"enabled":true}}` {
		t.Fatalf("configuration shared mutable caller bytes: %+v, %v", persisted, err)
	}
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", json.RawMessage(`{"label":"stale"}`)); err == nil {
		t.Fatal("stale module configuration overwrote its successor")
	}
	otherConfig, err := manager.ModuleConfig(t.Context(), other.ID)
	if err != nil || otherConfig.Version != "0" || string(otherConfig.Values) != "{}" {
		t.Fatalf("module configuration escaped its namespace: %+v, %v", otherConfig, err)
	}
	entries, err := manager.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range entries {
		if current.Manifest.ID == entry.Manifest.ID && current.Revision != entry.Revision {
			t.Fatal("configuration CAS rewrote the package catalog generation")
		}
	}
	if err := manager.catalog.SetEnabled(t.Context(), manifest.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "1", json.RawMessage(`{"label":"disabled"}`)); err != nil {
		t.Fatalf("disabled installed module lost configuration ownership: %v", err)
	}
	if manager.startCtx != nil || manager.closeDone != nil || manager.stopStarts != nil || len(manager.workers) != 0 {
		t.Fatal("configuration metadata APIs initialized a runtime lifecycle without workers")
	}
}

func TestModuleConfigCASRetiresIdleGroupWithoutAnotherAcquire(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := componentFixtureManifest()
	manifest.Services[1].Start = "eager"
	entry := installComponentFixture(t, manager, manifest)
	config, err := manager.ModuleConfig(t.Context(), manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newComponentFixture(t, manager, entry, config)
	if err := fixture.group.prepare(t.Context(), fixture.primary); err != nil {
		t.Fatal(err)
	}
	fixture.primary.group = fixture.group
	manager.mu.Lock()
	manager.startLifecycleLocked()
	worker := &managedWorker{entry: entry, config: config, client: fixture.primary}
	manager.workers[manifest.ID] = worker
	manager.mu.Unlock()
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	request, cancel := context.WithCancel(t.Context())
	defer cancel()
	fixture.stop = func(component string) {
		if component == sdk.PrimaryComponent {
			cancel()
		}
	}
	updated, err := manager.CompareAndSwapModuleConfig(request, manifest.ID, "0", json.RawMessage(`{"label":"new"}`))
	if err != nil || updated.Version != "1" {
		t.Fatalf("committed configuration retirement depended on its canceled caller: %+v %v", updated, err)
	}
	if request.Err() != context.Canceled {
		t.Fatal("idle primary was not stopped during the configuration CAS")
	}
	select {
	case <-fixture.group.closeDone:
	default:
		t.Fatal("idle configuration generation survived until another acquire")
	}
	manager.mu.Lock()
	published := manager.workers[manifest.ID]
	manager.mu.Unlock()
	worker.mu.Lock()
	retiring := worker.retiring
	worker.mu.Unlock()
	if published != nil || !retiring {
		t.Fatal("configuration CAS kept the old generation available for new leases")
	}
	fixture.mu.Lock()
	stopped := append([]string(nil), fixture.stopped...)
	fixture.mu.Unlock()
	if len(stopped) != 3 || stopped[0] != sdk.PrimaryComponent || stopped[1] != "helper" || stopped[2] != "dependency" {
		t.Fatalf("configuration CAS did not stop the idle primary and helpers: %v", stopped)
	}
}

func TestModuleConfigCASLetsAnActiveGroupDrain(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := componentFixtureManifest()
	entry := installComponentFixture(t, manager, manifest)
	config, err := manager.ModuleConfig(t.Context(), manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newComponentFixture(t, manager, entry, config)
	if err := fixture.group.prepare(t.Context(), fixture.primary); err != nil {
		t.Fatal(err)
	}
	fixture.primary.group = fixture.group
	worker := &managedWorker{entry: entry, config: config, client: fixture.primary, active: 1, idle: make(chan struct{})}
	manager.mu.Lock()
	manager.startLifecycleLocked()
	manager.workers[manifest.ID] = worker
	manager.mu.Unlock()
	var released sync.Once
	release := func() { released.Do(func() { releaseWorker(worker) }) }
	defer release()
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", json.RawMessage(`{"label":"new"}`)); err != nil {
		t.Fatal(err)
	}
	if fixture.group.ctx.Err() != nil || fixture.group.configSnapshot().Version != "0" {
		t.Fatal("configuration CAS canceled or retargeted the active old group")
	}
	select {
	case <-fixture.group.closeDone:
		t.Fatal("configuration CAS stopped the active old group before its lease drained")
	default:
	}
	release()
	wait, cancel := context.WithTimeout(t.Context(), componentCloseTimeout)
	defer cancel()
	select {
	case <-fixture.group.closeDone:
	case <-wait.Done():
		t.Fatal("retired group did not stop after its active lease drained")
	}
}

func TestModuleConfigAPIsRejectSDK10AndPreserveState(t *testing.T) {
	for _, cache := range []struct {
		name  string
		value json.RawMessage
	}{
		{name: "absent"},
		{name: "opaque", value: json.RawMessage(`"opaque SDK 1.0 cache"`)},
		{name: "object", value: json.RawMessage(`{"legacy":"cache"}`)},
	} {
		for _, api := range []string{"read", "public CAS", "revision CAS"} {
			t.Run(cache.name+"/"+api, func(t *testing.T) {
				manager := NewManager(t.TempDir())
				manifest := sdk.Manifest{
					Schema: 1, ID: "legacy-module", Version: "1.0.0", Runtime: "exec", Entry: "worker",
					Contract: sdk.Contract{
						Version: sdk.APIVersion{Major: 1, Minor: 0}, Capabilities: []sdk.Capability{sdk.OperationControl},
					},
				}
				entry := installComponentFixture(t, manager, manifest)
				scope := sdk.Scope{Plugin: manifest.ID}
				before, err := manager.store.StateGet(scope, moduleConfigKey)
				if err != nil {
					t.Fatal(err)
				}
				if cache.value != nil {
					before, err = manager.store.StateCAS(scope, moduleConfigKey, before.Version, cache.value)
					if err != nil {
						t.Fatal(err)
					}
				}
				expected := "0"
				if before.Version != 0 {
					expected = "1"
				}
				switch api {
				case "read":
					_, err = manager.ModuleConfig(t.Context(), manifest.ID)
				case "public CAS":
					_, err = manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, expected, json.RawMessage(`{"new":"configuration"}`))
				case "revision CAS":
					_, err = manager.compareAndSwapModuleConfig(t.Context(), manifest.ID, entry.Revision, expected, json.RawMessage(`{"new":"configuration"}`))
				}
				if err == nil {
					t.Fatal("SDK 1.0 module acquired the SDK 1.1 configuration namespace")
				}
				after, err := manager.store.StateGet(scope, moduleConfigKey)
				if err != nil || after.Version != before.Version || !bytes.Equal(after.Value, before.Value) {
					t.Fatalf("configuration API changed SDK 1.0 state: before=%+v after=%+v error=%v", before, after, err)
				}
			})
		}
	}
}

func TestModuleConfigConcurrentCASHasOneWinner(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := componentFixtureManifest()
	installComponentFixture(t, manager, manifest)
	const writers = 10
	start := make(chan struct{})
	results := make(chan error, writers)
	var pending sync.WaitGroup
	for range writers {
		pending.Add(1)
		go func() {
			defer pending.Done()
			<-start
			_, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", json.RawMessage(`{"winner":true}`))
			results <- err
		}()
	}
	close(start)
	pending.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("module configuration CAS winners = %d; want 1", success)
	}
}

func TestModuleConfigRejectsUnknownMalformedAndStaleInput(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := componentFixtureManifest()
	installComponentFixture(t, manager, manifest)
	for index, values := range []json.RawMessage{
		nil, json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`"string"`),
		json.RawMessage(`{"duplicate":1,"duplicate":2}`), json.RawMessage(`{} {}`),
		json.RawMessage(`{"cut":`), json.RawMessage(`{"deep":` + strings.Repeat("[", 130) + `0` + strings.Repeat("]", 130) + `}`),
		json.RawMessage(`{"large":"` + strings.Repeat("x", storeStateLimit) + `"}`),
	} {
		if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", values); err == nil {
			t.Fatalf("invalid module configuration case %d was written", index)
		}
	}
	for _, version := range []string{"", "00", "01", "-1", "1.0", "18446744073709551616"} {
		if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, version, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("invalid CAS version accepted: %q", version)
		}
	}
	for _, id := range []string{"missing-module", "../other-module"} {
		if _, err := manager.ModuleConfig(t.Context(), id); err == nil {
			t.Fatalf("unowned module configuration read: %s", id)
		}
		if _, err := manager.CompareAndSwapModuleConfig(t.Context(), id, "0", json.RawMessage(`{}`)); err == nil {
			t.Fatalf("unowned module configuration written: %s", id)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := manager.CompareAndSwapModuleConfig(ctx, manifest.ID, "0", json.RawMessage(`{}`)); err == nil {
		t.Fatal("canceled configuration mutation succeeded")
	}
	configuration, err := manager.ModuleConfig(t.Context(), manifest.ID)
	if err != nil || configuration.Version != "0" || !bytes.Equal(configuration.Values, []byte(`{}`)) {
		t.Fatalf("refused writes changed module configuration: %+v, %v", configuration, err)
	}
	if _, err := manager.store.StateCAS(sdk.Scope{Plugin: manifest.ID}, moduleConfigKey, 0, json.RawMessage(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ModuleConfig(t.Context(), manifest.ID); err == nil {
		t.Fatal("malformed persisted configuration became an empty snapshot")
	}
}

func TestModuleConfigPackageRevisionAndNotificationLockOrder(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := componentFixtureManifest()
	first := installComponentFixture(t, manager, manifest)
	changes.Lock()
	previous := changes.listeners
	notified := 0
	changes.listeners = []func(){func() {
		notified++
		root, err := manager.catalog.open(false)
		if err != nil {
			t.Error(err)
			return
		}
		defer root.Close()
		lock, err := root.OpenFile("catalog.lock", os.O_RDWR, 0o600)
		if err != nil {
			t.Error(err)
			return
		}
		defer lock.Close()
		held, err := filelock.TryLock(lock)
		if err != nil || !held {
			t.Errorf("configuration notification ran under the catalog lock: held=%v error=%v", held, err)
			return
		}
		filelock.Unlock(lock)
		if _, err := manager.List(); err != nil {
			t.Errorf("configuration listener could not list the catalog: %v", err)
		}
	}}
	changes.Unlock()
	t.Cleanup(func() {
		changes.Lock()
		changes.listeners = previous
		changes.Unlock()
	})
	if _, err := manager.compareAndSwapModuleConfig(t.Context(), manifest.ID, "stale-revision", "0", json.RawMessage(`{"label":"stale"}`)); err == nil {
		t.Fatal("configuration CAS accepted another package revision")
	}
	config, err := manager.ModuleConfig(t.Context(), manifest.ID)
	if err != nil || config.Version != "0" || notified != 0 {
		t.Fatalf("refused package revision changed configuration: %+v, %v; notifications=%d", config, err, notified)
	}
	if _, err := manager.compareAndSwapModuleConfig(t.Context(), manifest.ID, first.Revision, "0", json.RawMessage(`{"label":"first"}`)); err != nil {
		t.Fatal(err)
	}
	if notified != 1 {
		t.Fatalf("configuration notification count=%d; want 1", notified)
	}
	manifest.Version = "2.0.0"
	second := installComponentFixture(t, manager, manifest)
	if second.Revision == first.Revision {
		t.Fatal("package update retained its old revision")
	}
	if _, err := manager.compareAndSwapModuleConfig(t.Context(), manifest.ID, first.Revision, "1", json.RawMessage(`{"label":"wrong-package"}`)); err == nil {
		t.Fatal("old package view rewrote configuration after update")
	}
	config, err = manager.ModuleConfig(t.Context(), manifest.ID)
	if err != nil || config.Version != "1" || string(config.Values) != `{"label":"first"}` || notified != 1 {
		t.Fatalf("stale package view changed configuration: %+v, %v; notifications=%d", config, err, notified)
	}
}
