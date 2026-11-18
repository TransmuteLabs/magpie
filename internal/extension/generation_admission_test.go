package extension

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestAcquireRevisionRejectsInterleavedCachedGeneration(t *testing.T) {
	for _, change := range []string{"package", "configuration"} {
		t.Run(change, func(t *testing.T) {
			manager, original := fixtureManager(t)
			snapshot, err := manager.runtimeModuleConfig(original.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			old, err := manager.AcquireRevision(t.Context(), original.Manifest.ID, original.Revision, snapshot.Version)
			if err != nil {
				t.Fatal(err)
			}
			defer old.Close()

			// An acquisition has resolved this immutable snapshot, then another
			// request replaces the cached worker before its admission phase.
			current := original
			if change == "configuration" {
				if _, err := manager.CompareAndSwapModuleConfig(t.Context(), original.Manifest.ID, snapshot.Version,
					json.RawMessage(`{"generation":"replacement"}`)); err != nil {
					t.Fatal(err)
				}
			} else {
				source := fixturePackage(t)
				data, err := os.ReadFile(filepath.Join(source, sdk.ManifestFile))
				if err != nil {
					t.Fatal(err)
				}
				var manifest sdk.Manifest
				if err := json.Unmarshal(data, &manifest); err != nil {
					t.Fatal(err)
				}
				manifest.Version = "2.0.0"
				catalogTestWriteManifest(t, source, manifest)
				current, err = manager.Install(t.Context(), source)
				if err != nil {
					t.Fatal(err)
				}
			}
			latestConfig, err := manager.runtimeModuleConfig(current.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			replacement, err := manager.AcquireRevision(t.Context(), current.Manifest.ID, current.Revision, latestConfig.Version)
			if err != nil {
				t.Fatal(err)
			}
			defer replacement.Close()

			got, client, err := manager.acquireSelected(context.Background(), original.Manifest.ID, &original, snapshot)
			if err == nil {
				releaseWorker(got)
				t.Fatal("a stale package/configuration view was admitted into the replacement worker", client)
			}
			replacement.worker.mu.Lock()
			active := replacement.worker.active
			replacement.worker.mu.Unlock()
			if active != 1 {
				t.Fatalf("rejected acquisition changed the replacement lease count: %d", active)
			}
		})
	}
}
