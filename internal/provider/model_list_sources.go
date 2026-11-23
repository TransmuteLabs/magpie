package provider

import (
	"context"
	"os"
	"slices"
	"sync"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type extensionModelRoot struct {
	path string
	info os.FileInfo
}

func extensionModelRoots(ctx context.Context, installed bool) ([]extensionModelRoot, bool) {
	if !installed {
		return nil, true
	}
	inventory, err := providerExtensionInventoryContext(ctx)
	if err != nil {
		return nil, false
	}
	var roots []extensionModelRoot
	for _, selectedProvider := range inventory.providers {
		binding := selectedProvider.Account.extension
		if !binding.manifest.Contract.Has(sdk.ModelDiscovery) || binding.descriptor.AccountOnly() {
			continue
		}
		path := binding.host.Store().ModelCatalogPath(binding.scope.Plugin, binding.modelCatalogKey())
		if path == "" {
			return nil, false
		}
		info, err := os.Lstat(path)
		if err != nil && !os.IsNotExist(err) || info != nil && !info.Mode().IsRegular() {
			return nil, false
		}
		roots = append(roots, extensionModelRoot{path, info})
	}
	return roots, inventory.complete
}

func extensionModelRootsCurrent(roots []extensionModelRoot) bool {
	for _, root := range roots {
		info, err := os.Lstat(root.path)
		if os.IsNotExist(err) && root.info == nil {
			continue
		}
		if err != nil || !root.sameFile(info) {
			return false
		}
	}
	return true
}

func (root extensionModelRoot) sameFile(info os.FileInfo) bool {
	if root.info == nil || info == nil {
		return root.info == nil && info == nil
	}
	return os.SameFile(root.info, info) && root.info.Size() == info.Size() &&
		root.info.ModTime() == info.ModTime() && root.info.Mode() == info.Mode()
}

func sameExtensionModelRoot(first, second extensionModelRoot) bool {
	return first.path == second.path && first.sameFile(second.info)
}

type modelListObservation struct {
	stamp Stamp
	at    time.Time
	roots []extensionModelRoot
	ready bool
}

var modelListSources struct {
	sync.Mutex
	current modelListObservation
}

// RefreshModelListSources checks the SDK roots before rendering a gateway
// model list. A changed or unavailable observation drops held and kept
// projections before the list reads them again.
func RefreshModelListSources(ctx context.Context) {
	stamp := StampNow()
	modelListSources.Lock()
	current := modelListSources.current
	modelListSources.Unlock()
	if current.ready && current.stamp == stamp && time.Since(current.at) < keptFor && extensionModelRootsCurrent(current.roots) {
		return
	}

	roots, ready := extensionModelRoots(ctx, stamp.roots[extensionCatalogStampIndex].ok)
	modelListSources.Lock()
	defer modelListSources.Unlock()
	current = modelListSources.current
	if ctx.Err() != nil || current.stamp.gen > stamp.gen {
		return
	}
	if !ready || !slices.EqualFunc(current.roots, roots, sameExtensionModelRoot) {
		if !dropHeldAt(stamp.gen) {
			return
		}
		stamp.gen++
	}
	modelListSources.current = modelListObservation{stamp: stamp, at: time.Now(), roots: roots, ready: ready}
}
