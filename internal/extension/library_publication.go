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
	entries, _, err := catalogRead(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Manifest.ID != publication.module {
			continue
		}
		if !activeRoot(entry) || entry.Revision != publication.revision {
			break
		}
		generation, err := readObserverEnablement(root, publication.module)
		if err != nil {
			return err
		}
		if generation == publication.enableGeneration && !publication.manager.isClosed() {
			return ctx.Err()
		}
		break
	}
	return errContributionRevoked
}
