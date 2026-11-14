//go:build cgo && (darwin || linux || freebsd || windows)

package extension

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const nativeImageLoadLimit = 64

type nativeImageRecord struct {
	image        *nativeLibrary
	err          error
	dependencies string
	directory    string
	entry        Installed
}

type nativeImageNamespace struct {
	path string
	root *os.Root
	info os.FileInfo
}

type nativeImageCache struct {
	mu           sync.Mutex
	namespaces   []nativeImageNamespace
	images       map[string][]nativeImageRecord
	loading      map[string]chan struct{}
	attempts     int
	limit        int
	restart      bool
	opaque       bool
	dependencies func(string) (string, error)
}

var nativeImages = newNativeImageCache(nativeImageLoadLimit)

func newNativeImageCache(limit int) *nativeImageCache {
	return &nativeImageCache{
		images: make(map[string][]nativeImageRecord), loading: make(map[string]chan struct{}),
		limit: limit, dependencies: nativeDependencyIdentity,
	}
}

func nativeRestartRequired() error {
	return nativeRestartFailure("Native image capacity is exhausted.")
}

func nativeRestartFailure(reason string) error {
	return &sdk.Error{
		Code: "native_restart_required", Outcome: "not-started",
		Message: reason + " Restart the Magpie application or gateway process; plugin reload or restart cannot unload Go runtimes.",
	}
}

func openInstalledNativeTransport(ctx context.Context, root string, entry Installed) (*moduleTransport, error) {
	if runtime.GOOS != "linux" {
		if err := NewCatalog(root).verify(ctx, entry); err != nil {
			return nil, err
		}
		return openNativeTransport(ctx, filepath.Join(root, entry.Directory, entry.Manifest.Entry))
	}
	return openInstalledNativeTransportWith(ctx, root, entry, nativeImages, loadNativeLibrary)
}

func openInstalledNativeTransportWith(ctx context.Context, root string, entry Installed, cache *nativeImageCache,
	load func(string) (*nativeLibrary, error),
) (*moduleTransport, error) {
	return openNativeTransportWith(ctx, filepath.Join(root, entry.Directory, entry.Manifest.Entry),
		func(string) (*nativeLibrary, error) { return cache.acquire(ctx, root, entry, load) })
}

func (cache *nativeImageCache) authority(root string, entry Installed) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	directory, err := NewCatalog(absolute).open(false)
	if err != nil {
		return "", err
	}
	namespace, err := cache.registerNamespace(absolute, directory)
	if err != nil {
		return "", err
	}
	identity := struct {
		Namespace int
		Platform  string
		ABI       int
		Manifest  sdk.Manifest
		Files     map[string]string
		Source    *PackageSource
	}{namespace, runtime.GOOS + "/" + runtime.GOARCH, 1, entry.Manifest, entry.Files, entry.Source}
	body, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func (cache *nativeImageCache) registerNamespace(absolute string, directory *os.Root) (int, error) {
	info, err := directory.Stat(".")
	if err != nil {
		directory.Close()
		return -1, err
	}
	namespace := -1
	cache.mu.Lock()
	for index, existing := range cache.namespaces {
		if existing.path == absolute && os.SameFile(existing.info, info) {
			namespace = index
			break
		}
	}
	if namespace < 0 {
		if len(cache.namespaces) >= cache.limit {
			cache.mu.Unlock()
			directory.Close()
			return -1, nativeRestartRequired()
		}
		namespace = len(cache.namespaces)
		cache.namespaces = append(cache.namespaces, nativeImageNamespace{path: absolute, root: directory, info: info})
	} else {
		directory.Close()
	}
	cache.mu.Unlock()
	return namespace, nil
}

func (cache *nativeImageCache) acquire(ctx context.Context, root string, entry Installed,
	load func(string) (*nativeLibrary, error),
) (*nativeLibrary, error) {
	catalog := NewCatalog(root)
	if err := catalog.verify(ctx, entry); err != nil {
		return nil, err
	}
	key, err := cache.authority(root, entry)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := catalog.verify(ctx, entry); err != nil {
			return nil, err
		}
		dependencies, _ := cache.dependencies(filepath.Join(root, entry.Directory, entry.Manifest.Entry))
		cache.mu.Lock()
		for _, record := range cache.images[key] {
			if dependencies != "" && record.dependencies == dependencies ||
				dependencies == "" && record.dependencies == "" && record.entry.Revision == entry.Revision {
				cache.mu.Unlock()
				if err := NewCatalog(record.directory).verify(ctx, record.entry); err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					return nil, nativeRestartFailure("The retained native package no longer matches its verified authority.")
				}
				return record.image, record.err
			}
		}
		var waiting chan struct{}
		for _, active := range cache.loading {
			waiting = active
			break
		}
		if waiting != nil {
			cache.mu.Unlock()
			select {
			case <-waiting:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if cache.opaque || dependencies == "" && cache.attempts != 0 {
			cache.mu.Unlock()
			return nil, nativeRestartFailure("Native dependency equivalence cannot be established for another image.")
		}
		if cache.restart || cache.attempts >= cache.limit {
			cache.mu.Unlock()
			return nil, nativeRestartRequired()
		}
		waiting = make(chan struct{})
		cache.loading[key] = waiting
		cache.mu.Unlock()
		image, err := cache.load(ctx, catalog, entry, key, load)
		cache.mu.Lock()
		delete(cache.loading, key)
		close(waiting)
		cache.mu.Unlock()
		return image, err
	}
}

func (cache *nativeImageCache) load(ctx context.Context, catalog *Catalog, entry Installed, key string,
	load func(string) (*nativeLibrary, error),
) (*nativeLibrary, error) {
	entry, err := catalogClone(entry)
	if err != nil {
		return nil, err
	}
	directory, filename, err := nativeImageSnapshot(ctx, catalog, entry)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		os.RemoveAll(directory)
		return nil, err
	}
	cache.mu.Lock()
	if cache.restart || cache.attempts >= cache.limit {
		cache.mu.Unlock()
		os.RemoveAll(directory)
		return nil, nativeRestartRequired()
	}
	cache.attempts++
	cache.mu.Unlock()
	image, err := load(filename)
	dependencies, _ := cache.dependencies(filename)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if dependencies == "" {
		cache.opaque = true
	}
	if err != nil {
		var failure *sdk.Error
		if errors.As(err, &failure) && failure.Code == "native_restart_required" {
			cache.restart = true
		}
	}
	cache.images[key] = append(cache.images[key], nativeImageRecord{
		image: image, err: err, dependencies: dependencies, directory: directory, entry: entry,
	})
	return image, err
}

func nativeImageSnapshot(ctx context.Context, catalog *Catalog, entry Installed) (string, string, error) {
	if err := catalogValidate(entry); err != nil {
		return "", "", err
	}
	input, err := catalog.open(false)
	if err != nil {
		return "", "", err
	}
	defer input.Close()
	for _, segment := range strings.Split(entry.Directory, "/") {
		info, err := input.Lstat(segment)
		if err != nil {
			return "", "", err
		}
		if !info.IsDir() {
			return "", "", errors.New("native package is not a real directory")
		}
		child, err := input.OpenRoot(segment)
		if err != nil {
			return "", "", err
		}
		defer child.Close()
		if err := catalogDirectoryIdentity(child, info); err != nil {
			return "", "", err
		}
		input = child
	}
	directory, err := os.MkdirTemp("", "magpie-native-image-")
	if err != nil {
		return "", "", err
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(directory)
		}
	}()
	if err := os.MkdirAll(filepath.Join(directory, filepath.FromSlash(entry.Directory)), 0o700); err != nil {
		return "", "", err
	}
	output, err := os.OpenRoot(filepath.Join(directory, filepath.FromSlash(entry.Directory)))
	if err != nil {
		return "", "", err
	}
	defer output.Close()
	files, err := catalogCopy(ctx, input, output)
	if err != nil {
		return "", "", err
	}
	if !maps.Equal(files, entry.Files) {
		return "", "", errors.New("native image snapshot differs from its verified package inventory")
	}
	if err := catalog.verify(ctx, entry); err != nil {
		return "", "", err
	}
	if err := NewCatalog(directory).verify(ctx, entry); err != nil {
		return "", "", err
	}
	keep = true
	return directory, filepath.Join(directory, filepath.FromSlash(entry.Directory), filepath.FromSlash(entry.Manifest.Entry)), nil
}
