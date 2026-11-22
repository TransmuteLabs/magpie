package extension

import (
	"context"
	"errors"
	"os"
)

type LibraryPublication struct {
	manager          *Manager
	module           string
	revision         string
	enableGeneration string
}

func (m *Manager) libraryPublication(entry Installed) LibraryPublication {
	return LibraryPublication{
		manager: m, module: entry.Manifest.ID, revision: entry.Revision, enableGeneration: entry.EnableGeneration,
	}
}

func (publication LibraryPublication) Lock(ctx context.Context) (func(), error) {
	m := publication.manager
	if m == nil || m.isClosed() {
		return nil, errContributionRevoked
	}
	root, err := m.catalog.open(false)
	if errors.Is(err, errCatalogRootMissing) {
		return nil, errContributionRevoked
	}
	if err != nil {
		return nil, err
	}
	unlock, err := catalogLock(ctx, root)
	if err != nil {
		root.Close()
		return nil, err
	}
	release := func() {
		unlock()
		root.Close()
	}
	if err := publication.verifyLocked(ctx, root); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (publication LibraryPublication) verifyLocked(ctx context.Context, root *os.Root) error {
	view, err := publication.manager.catalog.readView(root)
	if err != nil {
		return err
	}
	entry, found := view.lookup(publication.module)
	if !found || !activeRoot(entry) || entry.Revision != publication.revision {
		return errContributionRevoked
	}
	entry = view.enabledEntry(entry)
	if entry.observerEnablementErr != nil {
		return entry.observerEnablementErr
	}
	if entry.EnableGeneration != publication.enableGeneration || publication.manager.isClosed() {
		return errContributionRevoked
	}
	return ctx.Err()
}
