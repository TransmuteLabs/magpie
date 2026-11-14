//go:build cgo && (darwin || linux || freebsd || windows)

package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func nativeImageFixture(t *testing.T) (*Catalog, string, Installed) {
	t.Helper()
	source := catalogTestPackage(t, "native-cache-fixture", "1.0.0")
	var manifest sdk.Manifest
	body, err := os.ReadFile(filepath.Join(source, sdk.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Runtime, manifest.Args = "native", nil
	catalogTestWriteManifest(t, source, manifest)
	catalog := NewCatalog(filepath.Join(t.TempDir(), "catalog"))
	return catalog, source, catalogTestInstall(t, catalog, source)
}

func nativeImageTestCache(t *testing.T, limit int) *nativeImageCache {
	t.Helper()
	cache := newNativeImageCache(limit)
	cache.dependencies = func(string) (string, error) { return "verified-system-closure", nil }
	t.Cleanup(func() {
		cache.mu.Lock()
		waiting := make([]chan struct{}, 0, len(cache.loading))
		for _, active := range cache.loading {
			waiting = append(waiting, active)
		}
		cache.mu.Unlock()
		for _, active := range waiting {
			nativeTestReceive(t, active)
		}
		for _, records := range cache.images {
			for _, record := range records {
				os.RemoveAll(record.directory)
			}
		}
		for _, namespace := range cache.namespaces {
			namespace.root.Close()
		}
	})
	return cache
}

func TestNativeImageReuseRequiresCompleteVerifiedAuthority(t *testing.T) {
	catalog, source, original := nativeImageFixture(t)
	cache := nativeImageTestCache(t, 16)
	var loads int
	load := func(string) (*nativeLibrary, error) {
		loads++
		return &nativeLibrary{}, nil
	}
	first, err := cache.acquire(t.Context(), catalog.Root, original, load)
	if err != nil {
		t.Fatal(err)
	}
	reinstalled := catalogTestInstall(t, catalog, source)
	second, err := cache.acquire(t.Context(), catalog.Root, reinstalled, load)
	if err != nil || first != second || loads != 1 || original.Revision == reinstalled.Revision {
		t.Fatalf("identical package relocation did not reuse only the image: loads=%d err=%v", loads, err)
	}
	catalogTestWrite(t, filepath.Join(catalog.Root, reinstalled.Directory, ".hidden"), []byte("tampered"), 0o600)
	if _, err := cache.acquire(t.Context(), catalog.Root, reinstalled, load); err == nil || loads != 1 {
		t.Fatal("cache hit bypassed candidate package verification")
	}
	catalogTestWrite(t, filepath.Join(source, ".hidden"), []byte("new dependency or resource"), 0o600)
	changedFile := catalogTestInstall(t, catalog, source)
	if image, err := cache.acquire(t.Context(), catalog.Root, changedFile, load); err != nil || image == first || loads != 2 {
		t.Fatalf("different package file reused an image: loads=%d err=%v", loads, err)
	}
	changedManifest := changedFile.Manifest.Clone()
	changedManifest.Version = "1.0.1"
	catalogTestWriteManifest(t, source, changedManifest)
	changed := catalogTestInstall(t, catalog, source)
	if image, err := cache.acquire(t.Context(), catalog.Root, changed, load); err != nil || image == first || loads != 3 {
		t.Fatalf("different manifest reused an image: loads=%d err=%v", loads, err)
	}
	provenance := PackageSource{
		Reference: "github:fixture/native", Tag: "v1.0.1", Archive: "magpie-plugin_linux_amd64.zip",
		SHA256: strings.Repeat("1", 64),
	}
	published, err := catalog.install(t.Context(), source, catalogTestCheck, installOptions{source: &provenance})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.acquire(t.Context(), catalog.Root, published, load); err != nil || loads != 4 {
		t.Fatalf("different provenance reused an image: loads=%d err=%v", loads, err)
	}
	other := NewCatalog(filepath.Join(t.TempDir(), "other-catalog"))
	otherEntry := catalogTestInstall(t, other, source)
	if _, err := cache.acquire(t.Context(), other.Root, otherEntry, load); err != nil || loads != 5 {
		t.Fatalf("different host namespace reused an image: loads=%d err=%v", loads, err)
	}
}

func TestNativeImageConcurrentLoadAndFreshLogicalSessions(t *testing.T) {
	catalog, _, entry := nativeImageFixture(t)
	cache := nativeImageTestCache(t, 4)
	var loads, sessions atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	image := &nativeLibrary{
		open: func(uintptr) (uint64, error) { return uint64(sessions.Add(1)), nil },
		send: func(uint64, []byte) error { return nil }, close: func(uint64) error { return nil },
	}
	load := func(string) (*nativeLibrary, error) {
		loads.Add(1)
		close(started)
		<-release
		return image, nil
	}
	var group sync.WaitGroup
	failures := make(chan error, 16)
	for range 16 {
		group.Go(func() {
			loaded, err := cache.acquire(t.Context(), catalog.Root, entry, load)
			if err == nil {
				var transport *moduleTransport
				transport, err = openNativeTransportWith(t.Context(), "fixture",
					func(string) (*nativeLibrary, error) { return loaded, nil })
				if err == nil {
					transport.stop()
					err = transport.wait()
				}
			}
			failures <- err
		})
	}
	<-started
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := cache.acquire(canceled, catalog.Root, entry, load); !errors.Is(err, context.Canceled) {
		t.Errorf("cache wait lost cancellation: %v", err)
	}
	close(release)
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if loads.Load() != 1 || sessions.Load() != 16 {
		t.Fatalf("loads=%d sessions=%d, want one image and sixteen logical sessions", loads.Load(), sessions.Load())
	}
}

func TestNativeImageCanceledLoaderDoesNotBlockTransportRetirement(t *testing.T) {
	catalog, _, entry := nativeImageFixture(t)
	cache := nativeImageTestCache(t, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, release, ended := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		close(release)
		<-ended
	}()
	var opened atomic.Int32
	load := func(string) (*nativeLibrary, error) {
		close(started)
		<-release
		defer close(ended)
		return &nativeLibrary{
			open:  func(uintptr) (uint64, error) { opened.Add(1); return 1, nil },
			close: func(uint64) error { return nil },
		}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := openInstalledNativeTransportWith(ctx, catalog.Root, entry, cache, load)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("native image load lost cancellation:", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("transport retirement waited for a wedged native image initializer")
	}
	if opened.Load() != 0 {
		t.Fatal("canceled native image initialized a logical module session")
	}
}

func TestNativeImageDependencyChangesAndUnknownClosureDoNotAlias(t *testing.T) {
	catalog, source, entry := nativeImageFixture(t)
	cache := nativeImageTestCache(t, 4)
	closure := "system-v1"
	cache.dependencies = func(string) (string, error) { return closure, nil }
	loads := 0
	load := func(string) (*nativeLibrary, error) { loads++; return &nativeLibrary{}, nil }
	first, err := cache.acquire(t.Context(), catalog.Root, entry, load)
	if err != nil {
		t.Fatal(err)
	}
	closure = "system-v2"
	second, err := cache.acquire(t.Context(), catalog.Root, entry, load)
	if err != nil || first == second || loads != 2 {
		t.Fatalf("different dependency proof reused an image: loads=%d err=%v", loads, err)
	}
	cache.dependencies = func(string) (string, error) { return "", errors.New("unknown closure") }
	entry = catalogTestInstall(t, catalog, source)
	_, err = cache.acquire(t.Context(), catalog.Root, entry, load)
	var failure *sdk.Error
	if !errors.As(err, &failure) || failure.Code != "native_restart_required" || loads != 2 {
		t.Fatalf("unknown dependency proof reused or loaded another image: %v, loads=%d", err, loads)
	}
}

func TestNativeImageAdmissionLimitKeepsServingCachedImages(t *testing.T) {
	catalog, source, entry := nativeImageFixture(t)
	cache := nativeImageTestCache(t, 1)
	loads := 0
	load := func(string) (*nativeLibrary, error) { loads++; return &nativeLibrary{}, nil }
	first, err := cache.acquire(t.Context(), catalog.Root, entry, load)
	if err != nil {
		t.Fatal(err)
	}
	catalogTestWrite(t, filepath.Join(source, ".hidden"), []byte("new generation"), 0o600)
	changed := catalogTestInstall(t, catalog, source)
	_, err = cache.acquire(t.Context(), catalog.Root, changed, load)
	var failure *sdk.Error
	if !errors.As(err, &failure) || failure.Code != "native_restart_required" || failure.Outcome != "not-started" {
		t.Fatalf("native admission limit did not require host restart: %v", err)
	}
	if reused, err := cache.acquire(t.Context(), catalog.Root, entry, load); err != nil || reused != first || loads != 1 {
		t.Fatalf("image admission limit retired a usable old image: %v", err)
	}
}

func TestNativeImageTLSFailureStopsMissesButRetainsOldImages(t *testing.T) {
	catalog, source, entry := nativeImageFixture(t)
	cache := nativeImageTestCache(t, 8)
	loads := 0
	load := func(string) (*nativeLibrary, error) {
		loads++
		if loads == 1 {
			return &nativeLibrary{}, nil
		}
		return nil, nativeLoadError("cannot allocate memory in static TLS block")
	}
	first, err := cache.acquire(t.Context(), catalog.Root, entry, load)
	if err != nil {
		t.Fatal(err)
	}
	catalogTestWrite(t, filepath.Join(source, ".hidden"), []byte("new generation"), 0o600)
	changed := catalogTestInstall(t, catalog, source)
	for range 2 {
		var failure *sdk.Error
		if _, err := cache.acquire(t.Context(), catalog.Root, changed, load); !errors.As(err, &failure) ||
			failure.Code != "native_restart_required" {
			t.Fatalf("native TLS failure was retried or obscured: %v", err)
		}
	}
	if old, err := cache.acquire(t.Context(), catalog.Root, entry, load); err != nil || old != first || loads != 2 {
		t.Fatalf("TLS failure retired the old image or retried an exhausted loader: %v loads=%d", err, loads)
	}
}

func TestNativeImageFailedMappedLibraryDoesNotLoadRepeatedly(t *testing.T) {
	catalog, source, entry := nativeImageFixture(t)
	cache := nativeImageTestCache(t, 4)
	loads := 0
	failure := errors.New("native library requires ABI exports")
	load := func(string) (*nativeLibrary, error) { loads++; return nil, failure }
	for range 3 {
		entry = catalogTestInstall(t, catalog, source)
		if _, err := cache.acquire(t.Context(), catalog.Root, entry, load); !errors.Is(err, failure) {
			t.Fatal(err)
		}
	}
	if loads != 1 {
		t.Fatalf("mapped but invalid image loaded %d times", loads)
	}
}

func TestNativeImageUnknownClosureKeepsOnlyItsExactGeneration(t *testing.T) {
	catalog, source, entry := nativeImageFixture(t)
	cache := nativeImageTestCache(t, 4)
	cache.dependencies = func(string) (string, error) { return "", errors.New("origin-sensitive library") }
	loads := 0
	load := func(string) (*nativeLibrary, error) { loads++; return &nativeLibrary{}, nil }
	first, err := cache.acquire(t.Context(), catalog.Root, entry, load)
	if err != nil {
		t.Fatal(err)
	}
	if same, err := cache.acquire(t.Context(), catalog.Root, entry, load); err != nil || same != first || loads != 1 {
		t.Fatalf("exact native generation was unusable after its handshake: loads=%d err=%v", loads, err)
	}
	reinstalled := catalogTestInstall(t, catalog, source)
	var failure *sdk.Error
	if _, err := cache.acquire(t.Context(), catalog.Root, reinstalled, load); !errors.As(err, &failure) ||
		failure.Code != "native_restart_required" || loads != 1 {
		t.Fatalf("unknown origin closure was claimed equivalent across generations: loads=%d err=%v", loads, err)
	}
}

func TestNativeImageSnapshotRetainsPackageOrigin(t *testing.T) {
	catalog, _, entry := nativeImageFixture(t)
	directory, filename, err := nativeImageSnapshot(t.Context(), catalog, entry)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	if err := os.RemoveAll(filepath.Join(catalog.Root, entry.Directory)); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filename); err != nil || string(body) != "executable fixture" {
		t.Fatalf("native origin followed the retired installation: %q %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(directory, entry.Directory, ".hidden")); err != nil {
		t.Fatal("native snapshot omitted origin-dependent package material:", err)
	}
}

func TestNativeImageCacheRejectsModifiedRetainedPackage(t *testing.T) {
	catalog, _, entry := nativeImageFixture(t)
	cache := nativeImageTestCache(t, 4)
	loads := 0
	load := func(string) (*nativeLibrary, error) { loads++; return &nativeLibrary{}, nil }
	if _, err := cache.acquire(t.Context(), catalog.Root, entry, load); err != nil {
		t.Fatal(err)
	}
	key, err := cache.authority(catalog.Root, entry)
	if err != nil {
		t.Fatal(err)
	}
	catalogTestWrite(t, filepath.Join(cache.images[key][0].directory, entry.Directory, ".hidden"),
		[]byte("changed retained material"), 0o600)
	var failure *sdk.Error
	if _, err := cache.acquire(t.Context(), catalog.Root, entry, load); !errors.As(err, &failure) ||
		failure.Code != "native_restart_required" || loads != 1 {
		t.Fatalf("modified origin material retained cache authority or triggered a second load: %v", err)
	}
}

func TestNativeImageNamespaceFollowsAnchoredRootIdentity(t *testing.T) {
	catalog, source, entry := nativeImageFixture(t)
	cache := nativeImageTestCache(t, 4)
	key, err := cache.authority(catalog.Root, entry)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := catalog.open(false)
	if err != nil {
		t.Fatal(err)
	}
	retired := catalog.Root + "-retired"
	if err := os.Rename(catalog.Root, retired); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(retired) })
	replacement := catalogTestInstall(t, catalog, source)
	registered, err := cache.registerNamespace(catalog.Root, pinned)
	if err != nil || registered != 0 {
		t.Fatalf("rename between open and registration changed pinned root authority: index=%d err=%v", registered, err)
	}
	other, err := cache.authority(catalog.Root, replacement)
	if err != nil || key == other || len(cache.namespaces) != 2 {
		t.Fatalf("a different catalog inode inherited native authority: %s %s %v", key, other, err)
	}
	anchored, err := cache.namespaces[0].root.Stat(".")
	if err != nil || !os.SameFile(anchored, cache.namespaces[0].info) {
		t.Fatal("retained namespace evidence refers to a path instead of its pinned directory:", err)
	}
}

func TestNativeRestartRequiredErrorRecognizesOnlyTLSExhaustion(t *testing.T) {
	for _, detail := range []string{
		"load native library failed: cannot allocate memory in static TLS block",
		"cannot load any more object with static TLS",
	} {
		var failure *sdk.Error
		if err := nativeLoadError(detail); !errors.As(err, &failure) || failure.Code != "native_restart_required" ||
			failure.Outcome != "not-started" || !strings.Contains(failure.Message, "Magpie") {
			t.Fatalf("TLS exhaustion did not require a process restart: %v", err)
		}
	}
	for _, detail := range []string{"missing library", "missing ABI symbol", "permission denied", "TLS protocol failed"} {
		var failure *sdk.Error
		if err := nativeLoadError(detail); errors.As(err, &failure) || err.Error() != detail {
			t.Fatalf("ordinary load error became a restart policy error: %v", err)
		}
	}
}
