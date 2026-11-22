package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func catalogListFixture(test testing.TB, resources int) (*Catalog, Installed) {
	test.Helper()
	source := test.TempDir()
	manifest := sdk.Manifest{
		Schema: 1, ID: "sample", Version: "1.0.0", Runtime: "exec",
		Entry: "worker", Args: []string{"initial"},
		Contract: sdk.Contract{
			Version:      sdk.APIVersion{Major: 1, Minor: 8},
			Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.LedgerObserver},
		},
		Providers: []sdk.ProviderDescriptor{{
			ID: "vendor", Name: "Vendor", Protocols: []protocolext.Protocol{protocolext.Chat},
			Models: []sdk.Model{{ID: "example", Price: &sdk.ModelPrice{Input: 1}}},
		}},
		Ledger: &sdk.LedgerObservationDescriptor{TimeoutMs: 100},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), body, 0o600); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "worker"), []byte("fixture"), 0o700); err != nil {
		test.Fatal(err)
	}
	for index := range resources {
		name := filepath.Join(source, fmt.Sprintf("asset-%03d.txt", index))
		if err := os.WriteFile(name, []byte("asset"), 0o600); err != nil {
			test.Fatal(err)
		}
	}
	catalog := NewCatalog(test.TempDir())
	provenance := &PackageSource{
		Reference: "github:owner/sample", Tag: "v1.0.0",
		Archive: "magpie-plugin_linux_amd64.zip", SHA256: strings.Repeat("a", 64),
	}
	entry, err := catalog.install(test.Context(), source, catalogTestCheck, installOptions{source: provenance})
	if err != nil {
		test.Fatal(err)
	}
	return catalog, entry
}

func TestCatalogListWarmAllocations(test *testing.T) {
	catalog, _ := catalogListFixture(test, 128)
	read := func(reader *Catalog) {
		if entries, err := reader.List(); err != nil || len(entries) != 1 {
			test.Fatalf("catalog read returned %d entries: %v", len(entries), err)
		}
	}
	cold := testing.AllocsPerRun(10, func() { read(NewCatalog(catalog.Root)) })
	warm := testing.AllocsPerRun(10, func() { read(catalog) })
	if warm >= cold/2 {
		test.Fatalf("warm catalog still allocates like a strict reparse: warm %.0f, cold %.0f", warm, cold)
	}
}

func TestSelectGatewayWarmCatalogReadAllocation(test *testing.T) {
	measure := func(resources int) uint64 {
		catalog, _ := catalogListFixture(test, resources)
		manager := NewManager(catalog.Root)
		ctx := test.Context()
		defer manager.Close(ctx)
		selectGateway := func() {
			selection := manager.SelectGateway(ctx, "local")
			if selection.manager != manager || selection.HasHooks() || selection.HasMiddleware() {
				test.Fatal("expected a successful gateway selection without hooks or middleware")
			}
		}
		selectGateway()

		const calls = 128
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range calls {
			selectGateway()
		}
		runtime.ReadMemStats(&after)
		return (after.TotalAlloc - before.TotalAlloc) / calls
	}
	small, large := measure(0), measure(128)
	if large > small+(4<<10) {
		test.Fatalf("larger warm gateway catalog allocated %d B/call, small catalog %d B/call", large, small)
	}
}

func gatewayCatalogFixture(test testing.TB, resources int) (*Manager, Installed, string) {
	test.Helper()
	catalog, entry := catalogListFixture(test, 0)
	entry.Manifest.Contract.Capabilities = append(entry.Manifest.Contract.Capabilities, sdk.GatewayHooks)
	entry.Manifest.Hooks = &sdk.GatewayHookDescriptor{Request: true}
	for index := range resources {
		entry.Files[fmt.Sprintf("assets/resource-%04d.txt", index)] = entry.Files["worker"]
	}
	entry.Revision = catalogRevision(entry)
	root, err := os.OpenRoot(catalog.Root)
	if err != nil {
		test.Fatal(err)
	}
	defer root.Close()
	if _, err := catalogWrite(test.Context(), root, []Installed{entry}); err != nil {
		test.Fatal(err)
	}
	manager := NewManager(catalog.Root)
	test.Cleanup(func() { manager.Close(context.Background()) })
	return manager, entry, filepath.Join(catalog.Root, "catalog.json")
}

func TestSelectGatewayWarmLargeCatalogBoundsAllocation(test *testing.T) {
	manager, entry, filename := gatewayCatalogFixture(test, 2048)
	info, err := os.Stat(filename)
	if err != nil {
		test.Fatal(err)
	}
	if info.Size() < 2*catalogReadScratchLimit {
		test.Fatalf("catalog fixture is only %d bytes", info.Size())
	}
	selectGateway := func() {
		selection := manager.SelectGateway(test.Context(), "local")
		if len(selection.hooks) != 1 || selection.hooks[0].Revision != entry.Revision {
			test.Fatal("gateway selection lost the expected hook")
		}
	}
	selectGateway()

	const calls = 128
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range calls {
		selectGateway()
	}
	runtime.ReadMemStats(&after)
	allocated := (after.TotalAlloc - before.TotalAlloc) / calls
	test.Logf("%d byte catalog: %d B/call", info.Size(), allocated)
	if allocated > catalogReadScratchLimit {
		test.Fatalf("warm gateway selection allocated %d B/call, want at most %d", allocated, catalogReadScratchLimit)
	}
}

func TestSelectGatewayLargeCatalogChecksCurrentBytesAndEnablement(test *testing.T) {
	manager, entry, filename := gatewayCatalogFixture(test, 2048)
	original, err := os.ReadFile(filename)
	if err != nil {
		test.Fatal(err)
	}
	stamp := time.Unix(1700000000, 0)
	if err := os.Chtimes(filename, stamp, stamp); err != nil {
		test.Fatal(err)
	}
	info, err := os.Stat(filename)
	if err != nil {
		test.Fatal(err)
	}
	assertHook := func(revision, generation string) {
		test.Helper()
		selection := manager.SelectGateway(test.Context(), "local")
		if len(selection.hooks) != 1 || selection.hooks[0].Revision != revision || selection.hooks[0].EnableGeneration != generation {
			test.Fatalf("gateway selection did not see revision %q and enablement %q: %d hooks", revision, generation, len(selection.hooks))
		}
	}
	replace := func(data []byte) {
		test.Helper()
		if len(data) != len(original) {
			test.Fatalf("replacement changes fixture size: %d != %d", len(data), len(original))
		}
		catalogTestWrite(test, filename, data, 0o600)
		if err := os.Chtimes(filename, stamp, stamp); err != nil {
			test.Fatal(err)
		}
		current, err := os.Stat(filename)
		if err != nil {
			test.Fatal(err)
		}
		if !os.SameFile(info, current) || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
			test.Fatal("replacement changed the catalog identity, size or timestamp")
		}
	}

	assertHook(entry.Revision, "")
	entry.Manifest.Args[0] = "updated"
	entry.Revision = catalogRevision(entry)
	replacement, err := json.Marshal([]Installed{entry})
	if err != nil {
		test.Fatal(err)
	}
	replace(replacement)
	assertHook(entry.Revision, "")

	marker := filepath.Join(manager.catalog.Root, observerEnablementFile(entry.Manifest.ID))
	const generation = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	catalogTestWrite(test, marker, []byte(`"`+generation+`"`), 0o600)
	assertHook(entry.Revision, generation)

	broken := bytes.Clone(replacement)
	broken[0] = '{'
	replace(broken)
	if selection := manager.SelectGateway(test.Context(), "local"); selection.manager != nil || manager.catalog.listSnapshot.Load() != nil {
		test.Fatal("invalid current catalog left the previous selection available")
	}
	replace(replacement)
	assertHook(entry.Revision, generation)
}

func BenchmarkSelectGatewayCatalog(bench *testing.B) {
	for _, setup := range []struct {
		name      string
		resources int
		cold      bool
		changed   bool
		parallel  bool
	}{
		{name: "warm-small"},
		{name: "warm-large", resources: 2048},
		{name: "warm-large-parallel", resources: 2048, parallel: true},
		{name: "cold-large", resources: 2048, cold: true},
		{name: "changed-large", resources: 2048, changed: true},
	} {
		bench.Run(setup.name, func(bench *testing.B) {
			manager, entry, _ := gatewayCatalogFixture(bench, setup.resources)
			ctx := bench.Context()
			selectGateway := func() bool {
				selection := manager.SelectGateway(ctx, "local")
				return len(selection.hooks) == 1 && selection.hooks[0].Revision == entry.Revision
			}
			if !selectGateway() {
				bench.Fatal("initial gateway selection lost the expected hook")
			}
			mismatch := *manager.catalog.listSnapshot.Load()
			mismatch.digest[0] ^= 1
			bench.ReportAllocs()
			if setup.parallel {
				bench.ResetTimer()
				bench.RunParallel(func(iterations *testing.PB) {
					for iterations.Next() {
						if !selectGateway() {
							bench.Error("gateway selection lost the expected hook")
							return
						}
					}
				})
				return
			}
			for bench.Loop() {
				if setup.cold {
					manager.catalog.listSnapshot.Store(nil)
				} else if setup.changed {
					manager.catalog.listSnapshot.Store(&mismatch)
				}
				if !selectGateway() {
					bench.Fatal("gateway selection lost the expected hook")
				}
			}
		})
	}
}

func TestCatalogListReadsExistingCatalogWithoutLock(t *testing.T) {
	catalog, installed := catalogListFixture(t, 0)
	if err := os.Remove(filepath.Join(catalog.Root, "catalog.lock")); err != nil {
		t.Fatal(err)
	}
	entries, err := catalog.List()
	if err != nil || len(entries) != 1 || entries[0].Revision != installed.Revision {
		t.Fatalf("existing catalog without a lock: %v, %v", entries, err)
	}
}

func BenchmarkCatalogList128Resources(bench *testing.B) {
	catalog, _ := catalogListFixture(bench, 128)
	if _, err := catalog.List(); err != nil {
		bench.Fatal(err)
	}
	bench.ReportAllocs()
	for bench.Loop() {
		if entries, err := catalog.List(); err != nil || len(entries) != 1 {
			bench.Fatalf("catalog read returned %d entries: %v", len(entries), err)
		}
	}
}

func TestCatalogListOwnsEachResult(test *testing.T) {
	catalog, installed := catalogListFixture(test, 0)
	first, err := catalog.List()
	if err != nil || len(first) != 1 {
		test.Fatalf("initial catalog: %v, %v", first, err)
	}
	second, err := catalog.List()
	if err != nil || len(second) != 1 {
		test.Fatalf("second catalog: %v, %v", second, err)
	}
	first[0].Manifest.Args[0] = "different"
	first[0].Manifest.Contract.Capabilities[0] = sdk.OperationControl
	first[0].Manifest.Providers[0].Protocols[0] = protocolext.Messages
	first[0].Manifest.Providers[0].Models[0].Price.Input = 99
	first[0].Manifest.Ledger.TimeoutMs = 500
	first[0].Source.SHA256 = strings.Repeat("b", 64)
	delete(first[0].Files, "worker")
	_ = slices.DeleteFunc(first, func(Installed) bool { return true })

	after, err := catalog.List()
	if err != nil || len(after) != 1 || !reflect.DeepEqual(installed, after[0]) || !reflect.DeepEqual(installed, second[0]) {
		test.Fatalf("mutation escaped into another read: %v, %v, %v", second, after, err)
	}
	if err := catalog.Verify(after[0]); err != nil {
		test.Fatal(err)
	}
	catalogTestWrite(test, filepath.Join(catalog.Root, filepath.FromSlash(installed.Directory), "worker"), []byte("changed"), 0o700)
	latest, err := catalog.List()
	if err != nil || len(latest) != 1 {
		test.Fatalf("catalog metadata read after payload change: %v, %v", latest, err)
	}
	if err := catalog.Verify(latest[0]); err == nil || !strings.Contains(err.Error(), "failed its hash check") {
		test.Fatalf("warm catalog stopped verifying files: %v", err)
	}
}

func TestCatalogListRecoversAndReadsLiveMarkers(test *testing.T) {
	catalog, installed := catalogListFixture(test, 0)
	filename := filepath.Join(catalog.Root, "catalog.json")
	original, err := os.ReadFile(filename)
	if err != nil {
		test.Fatal(err)
	}
	check := func(generation string, markerError bool) {
		test.Helper()
		listed, err := catalog.List()
		if err != nil || len(listed) != 1 || listed[0].Revision != installed.Revision ||
			listed[0].EnableGeneration != generation || (listed[0].observerEnablementErr != nil) != markerError {
			test.Fatalf("unexpected catalog or marker: %v, %v", listed, err)
		}
	}
	check("", false)
	marker := filepath.Join(catalog.Root, observerEnablementFile(installed.Manifest.ID))
	const generation = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	catalogTestWrite(test, marker, []byte(`"`+generation+`"`), 0o600)
	check(generation, false)
	catalogTestWrite(test, marker, []byte(`{"unknown":true}`), 0o600)
	check("", true)
	if err := os.Remove(marker); err != nil {
		test.Fatal(err)
	}
	check("", false)

	for _, invalid := range []string{
		"[",
		strings.Replace(string(original), `"disabled":false`, `"disabled":false,"disabled":true`, 1),
		strings.Replace(string(original), `"disabled":false`, `"disabled":false,"unknown":true`, 1),
		strings.Replace(string(original), installed.Revision, strings.Repeat("0", 64), 1),
	} {
		catalogTestWrite(test, filename, []byte(invalid), 0o600)
		if entries, err := catalog.List(); err == nil || entries != nil {
			test.Fatalf("invalid file reused an old catalog: %v, %v", entries, err)
		}
		catalogTestWrite(test, filename, original, 0o600)
		check("", false)
	}
	if err := os.Remove(filename); err != nil {
		test.Fatal(err)
	}
	if entries, err := catalog.List(); err != nil || entries == nil || len(entries) != 0 {
		test.Fatalf("missing file reused an old catalog: %v, %v", entries, err)
	}
	if err := os.Mkdir(filename, 0o700); err != nil {
		test.Fatal(err)
	}
	if entries, err := catalog.List(); err == nil || entries != nil {
		test.Fatalf("non-file reused an old catalog: %v, %v", entries, err)
	}
	if err := os.Remove(filename); err != nil {
		test.Fatal(err)
	}
	catalogTestWrite(test, filename, original, 0o600)
	check("", false)
}

func TestCatalogListReleasesUnavailableSnapshot(test *testing.T) {
	for _, failure := range []string{"missing root", "missing file and lock", "missing file", "malformed JSON", "directory"} {
		test.Run(failure, func(test *testing.T) {
			catalog, installed := catalogListFixture(test, 32)
			manager := NewManager(catalog.Root)
			defer manager.Close(test.Context())
			listed, err := manager.ListContext(test.Context())
			if err != nil || len(listed) != 1 || listed[0].Revision != installed.Revision {
				test.Fatalf("warm catalog: %v, %v", listed, err)
			}
			previous := weak.Make(manager.catalog.listSnapshot.Load())
			if previous.Value() == nil {
				test.Fatal("fixture did not retain the indexed catalog")
			}

			filename := filepath.Join(catalog.Root, "catalog.json")
			original, err := os.ReadFile(filename)
			if err != nil {
				test.Fatal(err)
			}
			parked := filepath.Join(test.TempDir(), "catalog-root")
			if failure == "missing root" {
				if err := os.Rename(catalog.Root, parked); err != nil {
					test.Fatal(err)
				}
			} else if err := os.Remove(filename); err != nil {
				test.Fatal(err)
			}
			switch failure {
			case "missing file and lock":
				if err := os.Remove(filepath.Join(catalog.Root, "catalog.lock")); err != nil {
					test.Fatal(err)
				}
			case "malformed JSON":
				catalogTestWrite(test, filename, []byte("["), 0o600)
			case "directory":
				if err := os.Mkdir(filename, 0o700); err != nil {
					test.Fatal(err)
				}
			}

			listed, err = manager.ListContext(test.Context())
			if failure == "malformed JSON" || failure == "directory" {
				if err == nil || listed != nil {
					test.Fatalf("invalid publication: %v, %v", listed, err)
				}
			} else if err != nil || listed == nil || len(listed) != 0 {
				test.Fatalf("absent publication: %v, %v", listed, err)
			}
			if manager.catalog.listSnapshot.Load() != nil {
				test.Error("unavailable catalog still owns its previous decoded snapshot")
			}
			for range 3 {
				runtime.GC()
			}
			if previous.Value() != nil {
				test.Error("previous decoded snapshot survived collection")
			}

			switch failure {
			case "missing root":
				if err := os.Rename(parked, catalog.Root); err != nil {
					test.Fatal(err)
				}
			case "directory":
				if err := os.Remove(filename); err != nil {
					test.Fatal(err)
				}
				fallthrough
			default:
				catalogTestWrite(test, filename, original, 0o600)
			}
			listed, err = manager.ListContext(test.Context())
			if err != nil || len(listed) != 1 || listed[0].Revision != installed.Revision ||
				manager.catalog.listSnapshot.Load() == nil {
				test.Fatalf("catalog repair did not republish a valid snapshot: %v, %v", listed, err)
			}
		})
	}
}

func TestCatalogListKeepsSnapshotOnCanceledLock(test *testing.T) {
	catalog, _ := catalogListFixture(test, 0)
	manager := NewManager(catalog.Root)
	defer manager.Close(test.Context())
	if listed, err := manager.ListContext(test.Context()); err != nil || len(listed) != 1 {
		test.Fatalf("warm catalog: %v, %v", listed, err)
	}
	previous := manager.catalog.listSnapshot.Load()
	root, err := catalog.open(false)
	if err != nil {
		test.Fatal(err)
	}
	defer root.Close()
	unlock, err := catalogLock(test.Context(), root)
	if err != nil {
		test.Fatal(err)
	}
	defer unlock()
	if err := root.Remove("catalog.json"); err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(test.Context(), 30*time.Millisecond)
	defer cancel()
	if listed, err := manager.ListContext(ctx); !errors.Is(err, context.DeadlineExceeded) || listed != nil {
		test.Fatalf("catalog read through held lock: %v, %v", listed, err)
	}
	if manager.catalog.listSnapshot.Load() != previous {
		test.Fatal("a canceled read changed the validated catalog")
	}
}

func TestCatalogReadFromReplacedRootKeepsRepair(test *testing.T) {
	if runtime.GOOS == "windows" {
		test.Skip("open rooted directory cannot be renamed on Windows")
	}
	catalog, _ := catalogListFixture(test, 0)
	manager := NewManager(catalog.Root)
	defer manager.Close(test.Context())
	if listed, err := manager.ListContext(test.Context()); err != nil || len(listed) != 1 {
		test.Fatalf("warm catalog: %v, %v", listed, err)
	}
	root, err := catalog.open(false)
	if err != nil {
		test.Fatal(err)
	}
	defer root.Close()
	unlock, err := catalogLock(test.Context(), root)
	if err != nil {
		test.Fatal(err)
	}
	defer unlock()
	if err := root.Remove("catalog.json"); err != nil {
		test.Fatal(err)
	}
	if err := os.Rename(catalog.Root, filepath.Join(test.TempDir(), "old")); err != nil {
		test.Fatal(err)
	}
	peer := NewCatalog(catalog.Root)
	installed := catalogTestInstall(test, peer, catalogTestPackage(test, "sample", "1.0.1"))
	listed, err := manager.ListContext(test.Context())
	if err != nil || len(listed) != 1 || listed[0].Revision != installed.Revision {
		test.Fatalf("replacement catalog: %v, %v", listed, err)
	}
	repaired := manager.catalog.listSnapshot.Load()
	if view, err := manager.catalog.readView(root); err != nil || len(view.entries) != 0 {
		test.Fatalf("stale rooted absence: %v, %v", view, err)
	}
	if err := root.WriteFile("catalog.json", []byte("["), 0o600); err != nil {
		test.Fatal(err)
	}
	if _, err := manager.catalog.readView(root); err == nil {
		test.Fatal("stale rooted invalid data was accepted")
	}
	if manager.catalog.listSnapshot.Load() != repaired {
		test.Fatal("the old rooted read evicted the replacement's validated snapshot")
	}
}

func TestCatalogListSeesAtomicReplacementWithSameSizeAndTime(test *testing.T) {
	const childRoot = "MAGPIE_TEST_CATALOG_REPLACEMENT_ROOT"
	if directory := os.Getenv(childRoot); directory != "" {
		root, err := os.OpenRoot(directory)
		if err != nil {
			test.Fatal(err)
		}
		defer root.Close()
		unlock, err := catalogLock(test.Context(), root)
		if err != nil {
			test.Fatal(err)
		}
		defer unlock()
		if err := root.Rename("replacement.json", "catalog.json"); err != nil {
			test.Fatal(err)
		}
		return
	}

	catalog := NewCatalog(test.TempDir())
	before := catalogTestInstall(test, catalog, catalogTestPackage(test, "sample", "1.0.0"))
	filename := filepath.Join(catalog.Root, "catalog.json")
	original, err := os.ReadFile(filename)
	if err != nil {
		test.Fatal(err)
	}
	after := catalogTestInstall(test, catalog, catalogTestPackage(test, "sample", "1.0.1"))
	replacement := filepath.Join(catalog.Root, "replacement.json")
	if err := os.Rename(filename, replacement); err != nil {
		test.Fatal(err)
	}
	catalogTestWrite(test, filename, original, 0o600)
	stamp := time.Unix(1700000000, 0)
	for _, name := range []string{filename, replacement} {
		if err := os.Chtimes(name, stamp, stamp); err != nil {
			test.Fatal(err)
		}
		info, err := os.Stat(name)
		if err != nil {
			test.Fatal(err)
		}
		if info.Size() != int64(len(original)) || !info.ModTime().Equal(stamp) {
			test.Fatalf("fixture metadata differs: %d bytes, %v", info.Size(), info.ModTime())
		}
	}
	if entries, err := catalog.List(); err != nil || len(entries) != 1 || entries[0].Revision != before.Revision {
		test.Fatalf("catalog did not warm the original revision: %v, %v", entries, err)
	}

	var readers sync.WaitGroup
	stop := make(chan struct{})
	for range 3 {
		readers.Go(func() {
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				entries, err := catalog.List()
				if err != nil || len(entries) != 1 || entries[0].Revision != before.Revision && entries[0].Revision != after.Revision {
					test.Errorf("concurrent catalog read returned an incomplete generation: %v, %v", entries, err)
					return
				}
				select {
				case <-stop:
					return
				case <-ticker.C:
				}
			}
		})
	}
	ctx, cancel := context.WithTimeout(test.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCatalogListSeesAtomicReplacementWithSameSizeAndTime$")
	command.Env = append(os.Environ(), childRoot+"="+catalog.Root)
	output, err := command.CombinedOutput()
	close(stop)
	readers.Wait()
	if err != nil {
		test.Fatalf("catalog writer: %v\n%s", err, output)
	}
	if entries, err := catalog.List(); err != nil || len(entries) != 1 || entries[0].Revision != after.Revision {
		test.Fatalf("same-size and same-time replacement was ignored: %v, %v", entries, err)
	}
}
