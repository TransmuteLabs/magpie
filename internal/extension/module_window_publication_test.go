package extension

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestWindowDiscoveryDoesNotBlockPeerEnablement(t *testing.T) {
	for _, fixture := range []struct {
		name        string
		list        func(*Manager, context.Context) ([]WindowModule, error)
		cycleWindow bool
	}{
		{name: "change_poll", list: (*Manager).Windows},
		{name: "metadata", list: (*Manager).WindowsWithMetadata},
		{name: "metadata_after_window_cycle", list: (*Manager).WindowsWithMetadata, cycleWindow: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			manager := NewManager(t.TempDir())
			t.Cleanup(func() { managerReleaseClose(t, manager) })
			window := catalogTestInstall(t, manager.catalog, moduleWindowPackage(t, "1.0.0"))
			other := catalogTestInstall(t, manager.catalog, catalogTestPackage(t, "other-module", "1.0.0"))
			peer := NewCatalog(manager.Namespace())
			release := sync.OnceFunc(holdPrivateStoreTestLock(t, manager.store))
			defer release()

			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				type result struct {
					modules []WindowModule
					err     error
				}
				done := make(chan result, 1)
				go func() {
					modules, err := fixture.list(manager, ctx)
					done <- result{modules, err}
				}()
				synctest.Wait()
				select {
				case read := <-done:
					t.Fatalf("window discovery returned before Store release: %v", read.err)
				default:
				}

				id := other.Manifest.ID
				if fixture.cycleWindow {
					id = window.Manifest.ID
				}
				deadline, stop := context.WithTimeout(t.Context(), time.Second)
				defer stop()
				err := peer.SetEnabled(deadline, id, false)
				if err == nil && fixture.cycleWindow {
					err = peer.SetEnabled(deadline, id, true)
				}
				release()
				read := <-done
				if err != nil {
					t.Fatalf("peer enablement could not commit while the private Store was held: %v", err)
				}
				if fixture.cycleWindow {
					if !errors.Is(read.err, errContributionRevoked) || len(read.modules) != 0 {
						t.Fatalf("window enable cycle published the old selection: %+v, %v", read.modules, read.err)
					}
					read.modules, read.err = fixture.list(manager, t.Context())
				}
				if read.err != nil || len(read.modules) != 1 || read.modules[0].ID != window.Manifest.ID {
					t.Fatalf("window discovery returned %+v, %v", read.modules, read.err)
				}
			})
		})
	}
}

func TestWindowConfigReadRejectsPeerEnablementChange(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		reenable bool
	}{
		{name: "disable"},
		{name: "disable_then_enable", reenable: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			manager := NewManager(t.TempDir())
			t.Cleanup(func() { managerReleaseClose(t, manager) })
			window := catalogTestInstall(t, manager.catalog, moduleWindowPackage(t, "1.0.0"))
			id := window.Manifest.ID
			revision := windowRevision(window.Revision, "0")
			peer := NewCatalog(manager.Namespace())
			release := sync.OnceFunc(holdPrivateStoreTestLock(t, manager.store))
			defer release()

			synctest.Test(t, func(t *testing.T) {
				type result struct {
					config sdk.ConfigSnapshot
					err    error
				}
				done := make(chan result, 1)
				go func() {
					config, err := manager.ReadWindowConfig(t.Context(), id, revision)
					done <- result{config, err}
				}()
				synctest.Wait()
				select {
				case read := <-done:
					t.Fatalf("configuration read returned before Store release: %+v", read)
				default:
				}

				deadline, stop := context.WithTimeout(t.Context(), time.Second)
				defer stop()
				err := peer.SetEnabled(deadline, id, false)
				if err == nil && fixture.reenable {
					err = peer.SetEnabled(deadline, id, true)
				}
				release()
				read := <-done
				if err != nil {
					t.Fatalf("peer enablement could not commit while the private Store was held: %v", err)
				}
				if !errors.Is(read.err, errContributionRevoked) || read.config.Version != "" || len(read.config.Values) != 0 {
					t.Fatalf("configuration read after peer %s returned version %q, values %s, error %v",
						fixture.name, read.config.Version, read.config.Values, read.err)
				}
				if !fixture.reenable {
					if err := peer.SetEnabled(deadline, id, true); err != nil {
						t.Fatal(err)
					}
				}
				config, err := manager.ReadWindowConfig(t.Context(), id, revision)
				if err != nil || config.Version != "0" || string(config.Values) != "{}" {
					t.Fatalf("fresh configuration read returned %+v, %v", config, err)
				}
			})
		})
	}
}
