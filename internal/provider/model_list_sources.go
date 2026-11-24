package provider

import (
	"context"
	"os"
	"slices"
	"sync"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
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
	complete := inventory.complete
	for _, selectedProvider := range inventory.providers {
		binding := selectedProvider.Account.extension
		if !binding.manifest.Contract.Has(sdk.ModelDiscovery) || binding.descriptor.AccountOnly() {
			continue
		}
		path := binding.host.Store().ModelCatalogPath(binding.scope.Plugin, binding.modelCatalogKey())
		if path == "" {
			complete = false
			continue
		}
		info, err := os.Lstat(path)
		if err != nil && !os.IsNotExist(err) || info != nil && !info.Mode().IsRegular() {
			complete = false
			continue
		}
		roots = append(roots, extensionModelRoot{path, info})
	}
	return roots, complete
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

// ModelListObservation records the catalog and its observed SDK roots.
type ModelListObservation struct {
	stamp Stamp
	at    time.Time
	roots []extensionModelRoot
	ready bool
}

// Current checks the catalog stamp and known roots without rereading module
// configuration. An incomplete observation still checks the roots it found.
func (observation ModelListObservation) Current(ctx context.Context) bool {
	return ctx.Err() == nil && extensionModelRootsCurrent(observation.roots) &&
		observation.stamp == StampNow()
}

func (observation ModelListObservation) CurrentWithAccountSources(ctx context.Context) bool {
	if observation.stamp.roots[extensionCatalogStampIndex].ok {
		accounts, valid := extensionAccountObservations.observeContext(ctx, extensionhost.AccountStamp)
		if valid && ctx.Err() == nil {
			held.Lock()
			holdAccountSourcesLocked(accounts)
			held.Unlock()
		}
	}
	return observation.Current(ctx)
}

var modelListSources struct {
	sync.Mutex
	current ModelListObservation
}

// RefreshModelListSources checks the SDK roots before rendering a gateway
// model list. A changed or unavailable observation drops held and kept
// projections before the list reads them again.
func RefreshModelListSources(ctx context.Context) ModelListObservation {
	stamp := StampNow()
	modelListSources.Lock()
	current := modelListSources.current
	modelListSources.Unlock()
	if current.ready && current.stamp == stamp && time.Since(current.at) < keptFor && extensionModelRootsCurrent(current.roots) {
		return current
	}

	roots, ready := extensionModelRoots(ctx, stamp.roots[extensionCatalogStampIndex].ok)
	modelListSources.Lock()
	defer modelListSources.Unlock()
	current = modelListSources.current
	if ctx.Err() != nil || current.stamp.gen > stamp.gen {
		return ModelListObservation{}
	}
	if !ready || !slices.EqualFunc(current.roots, roots, sameExtensionModelRoot) {
		if !dropHeldAt(stamp.gen) {
			return ModelListObservation{}
		}
		stamp.gen++
	}
	observation := ModelListObservation{stamp: stamp, at: time.Now(), roots: roots, ready: ready}
	modelListSources.current = observation
	return observation
}
