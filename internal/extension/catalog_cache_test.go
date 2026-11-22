package extension

import (
	"context"
	"encoding/json"
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
