package extension

import (
	"errors"
	"testing"
)

func TestActiveModuleSelectionStopsWithRegistry(t *testing.T) {
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { managerReleaseClose(t, manager) })
	installed := catalogTestInstall(t, manager.catalog, moduleWindowPackage(t, "1.0.0"))
	id := installed.Manifest.ID
	if entry, err := manager.ActiveModule(t.Context(), id); err != nil || entry.Revision != installed.Revision {
		t.Fatalf("live registry selection: %s, %v", entry.Revision, err)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ActiveModules(t.Context()); !errors.Is(err, errContributionRevoked) {
		t.Fatalf("closed registry list returned %v", err)
	}
	if _, err := manager.ActiveModule(t.Context(), id); !errors.Is(err, errContributionRevoked) {
		t.Fatalf("closed registry selected an installed module: %v", err)
	}
}
