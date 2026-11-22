package extension

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestModuleSettingsReadDoesNotBlockPeerEnablement(t *testing.T) {
	for _, reader := range moduleSettingsReaders() {
		t.Run(reader.name, func(t *testing.T) {
			for _, fixture := range []struct {
				name     string
				selected bool
				reenable bool
			}{
				{name: "unrelated_disable"},
				{name: "selected_disable", selected: true},
				{name: "selected_off_on", selected: true, reenable: true},
			} {
				t.Run(fixture.name, func(t *testing.T) {
					manager, entry := moduleSettingsReadFixture(t)
					id := entry.Manifest.ID
					other := entry.Manifest.Clone()
					other.ID = "other-module"
					other.Settings[0].Default = "sibling"
					installComponentFixture(t, manager, other)
					peer := NewCatalog(manager.Namespace())
					release := sync.OnceFunc(holdPrivateStoreTestLock(t, manager.store))
					defer release()

					synctest.Test(t, func(t *testing.T) {
						done := make(chan moduleSettingsRead, 1)
						go func() { done <- reader.read(t.Context(), manager, entry) }()
						synctest.Wait()
						select {
						case result := <-done:
							t.Fatalf("settings read returned before Store release: %+v", result)
						default:
						}
						target := other.ID
						if fixture.selected {
							target = id
						}
						ctx, cancel := context.WithTimeout(t.Context(), time.Second)
						defer cancel()
						err := peer.SetEnabled(ctx, target, false)
						if err == nil && fixture.reenable {
							err = peer.SetEnabled(ctx, target, true)
						}
						release()
						result := <-done
						if err != nil {
							t.Fatalf("peer enablement could not commit while settings waited for the private Store: %v", err)
						}
						if result.rows != nil {
							sibling := result.rows[other.ID]
							if fixture.selected {
								if sibling.Err != nil || sibling.Snapshot.ConfigVersion != "0" ||
									string(sibling.Snapshot.Values["label"]) != `"sibling"` {
									t.Fatalf("revoked row changed its sibling: %+v", sibling)
								}
							} else if !errors.Is(sibling.Err, ErrCatalogConflict) || sibling.Snapshot.Values != nil {
								t.Fatalf("settings exposed the disabled sibling: %+v", sibling)
							}
						}
						if fixture.selected {
							if !errors.Is(result.Err, ErrCatalogConflict) || result.Snapshot.Values != nil {
								t.Fatalf("settings exposed the revoked selection: %+v", result)
							}
							if !fixture.reenable {
								entries, options, err := manager.ListWithSettings(t.Context())
								_, present := options[id]
								disabled := slices.ContainsFunc(entries, func(entry Installed) bool {
									return entry.Manifest.ID == id && entry.Disabled
								})
								if err != nil || len(entries) != 2 || !disabled || present {
									t.Fatalf("disabled plugin's display row or options changed: %+v, %+v, %v", entries, options, err)
								}
								if err := peer.SetEnabled(ctx, id, true); err != nil {
									t.Fatal(err)
								}
							}
							var err error
							entry, err = manager.ActiveModule(t.Context(), id)
							if err != nil {
								t.Fatal(err)
							}
							result = reader.read(t.Context(), manager, entry)
						}
						if result.Err != nil || result.Snapshot.PackageRevision != entry.Revision ||
							result.Snapshot.EnableGeneration != entry.EnableGeneration ||
							result.Snapshot.ConfigVersion != "0" || string(result.Snapshot.Values["label"]) != `"public"` {
							t.Fatalf("settings projection changed: %+v", result)
						}
					})
				})
			}
		})
	}
}

func TestModuleSettingsReadKeepsVersionWithPeerCAS(t *testing.T) {
	for _, reader := range moduleSettingsReaders() {
		t.Run(reader.name, func(t *testing.T) {
			manager, entry := moduleSettingsReadFixture(t)
			peer := NewManager(manager.Namespace())
			t.Cleanup(func() { managerReleaseClose(t, peer) })
			release := sync.OnceFunc(holdPrivateStoreTestLock(t, manager.store))
			defer release()

			synctest.Test(t, func(t *testing.T) {
				saved := make(chan error, 1)
				go func() {
					_, err := peer.CompareAndSwapModuleConfig(t.Context(), entry.Manifest.ID, "0", json.RawMessage(`{"label":"committed"}`))
					saved <- err
				}()
				synctest.Wait()
				select {
				case err := <-saved:
					t.Fatalf("peer configuration write returned before Store release: %v", err)
				default:
				}
				done := make(chan moduleSettingsRead, 1)
				go func() { done <- reader.read(t.Context(), manager, entry) }()
				synctest.Wait()
				release()
				if err := <-saved; err != nil {
					t.Fatal(err)
				}
				result := <-done
				if result.Err != nil || result.Snapshot.PackageRevision != entry.Revision ||
					result.Snapshot.EnableGeneration != entry.EnableGeneration ||
					result.Snapshot.ConfigVersion != "1" || string(result.Snapshot.Values["label"]) != `"committed"` {
					t.Fatalf("settings revision and values disagree after peer CAS: %+v", result)
				}
			})
		})
	}
}

func TestModuleSettingsReadCancelsWhileWaitingForStore(t *testing.T) {
	for _, reader := range moduleSettingsReaders() {
		t.Run(reader.name, func(t *testing.T) {
			manager, entry := moduleSettingsReadFixture(t)
			release := holdPrivateStoreTestLock(t, manager.store)
			defer release()

			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan moduleSettingsRead, 1)
				go func() { done <- reader.read(ctx, manager, entry) }()
				synctest.Wait()
				select {
				case result := <-done:
					t.Fatalf("settings read returned before cancellation: %+v", result)
				default:
				}
				cancel()
				synctest.Wait()
				select {
				case result := <-done:
					if !errors.Is(result.Err, context.Canceled) || result.Snapshot.Values != nil || result.rows != nil {
						t.Fatalf("canceled read returned settings: %+v", result)
					}
				default:
					t.Fatal("canceled read still waits for the private Store")
				}
			})
		})
	}
}

func moduleSettingsReadFixture(t *testing.T) (*Manager, Installed) {
	t.Helper()
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { managerReleaseClose(t, manager) })
	manifest := componentFixtureManifest()
	manifest.Contract.Version.Minor = sdk.ModuleSettingsAPIMinor
	manifest.Settings = []sdk.FeatureSetting{{Name: "label", Title: "Label", Kind: sdk.FeatureSettingString, Default: "public"}}
	return manager, installComponentFixture(t, manager, manifest)
}

type moduleSettingsReader struct {
	name string
	read func(context.Context, *Manager, Installed) moduleSettingsRead
}

type moduleSettingsRead struct {
	ModuleSettingsResult
	rows map[string]ModuleSettingsResult
}

func moduleSettingsReaders() []moduleSettingsReader {
	return []moduleSettingsReader{
		{name: "list", read: func(ctx context.Context, manager *Manager, entry Installed) moduleSettingsRead {
			_, options, err := manager.ListWithSettings(ctx)
			if err != nil {
				return moduleSettingsRead{ModuleSettingsResult: ModuleSettingsResult{Err: err}}
			}
			return moduleSettingsRead{ModuleSettingsResult: options[entry.Manifest.ID], rows: options}
		}},
		{name: "single", read: func(ctx context.Context, manager *Manager, entry Installed) moduleSettingsRead {
			snapshot, err := manager.ModuleSettings(ctx, entry)
			return moduleSettingsRead{ModuleSettingsResult: ModuleSettingsResult{Snapshot: snapshot, Err: err}}
		}},
	}
}

func TestModuleSettingsProjectionPinsOwner(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := componentFixtureManifest()
	manifest.Contract.Version.Minor = sdk.ModuleSettingsAPIMinor
	manifest.Settings = []sdk.FeatureSetting{{Name: "label", Title: "Label", Kind: sdk.FeatureSettingString, Default: "public"}}
	installed := installComponentFixture(t, manager, manifest)
	options, err := manager.ModuleSettings(t.Context(), installed)
	if err != nil || string(options.Values["label"]) != `"public"` || options.ConfigVersion != "0" {
		t.Fatalf("first settings snapshot: %+v, %v", options, err)
	}
	if err := manager.SetEnabled(t.Context(), manifest.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetEnabled(t.Context(), manifest.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ModuleSettings(t.Context(), installed); !errors.Is(err, ErrCatalogConflict) {
		t.Errorf("settings snapshot survived a change in enablement: %v", err)
	}
	entries, err := manager.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("current catalog: %+v, %v", entries, err)
	}
	selected := entries[0]
	manifest.Version = "2.0.0"
	manifest.Settings = nil
	installComponentFixture(t, manager, manifest)
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), manifest.ID, "0", json.RawMessage(`{"label":"private"}`)); err != nil {
		t.Fatal(err)
	}
	if projection, err := manager.ModuleSettings(t.Context(), selected); !errors.Is(err, ErrCatalogConflict) || len(projection.Values) != 0 {
		t.Fatalf("stale settings descriptor projected replacement configuration: %+v, %v", projection, err)
	}
}

func TestClosedModuleOwnerCannotProjectOrPublishSettings(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := componentFixtureManifest()
	manifest.Contract.Version.Minor = sdk.ModuleSettingsAPIMinor
	manifest.Settings = []sdk.FeatureSetting{{Name: "label", Title: "Label", Kind: sdk.FeatureSettingString, Default: "public"}}
	entry := installComponentFixture(t, manager, manifest)
	selection, err := manager.ModuleSettings(t.Context(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ModuleSettings(t.Context(), entry); err == nil {
		t.Error("closed module owner projected settings")
	}
	if err := manager.CompareAndSwapModuleSettings(t.Context(), manifest.ID, selection.ModuleSettingsRevision, json.RawMessage(`{"label":"closed"}`)); err == nil {
		t.Error("closed module owner accepted a settings write")
	}
	current, err := NewManager(manager.root).ModuleSettings(t.Context(), entry)
	if err != nil || current.ConfigVersion != "0" || string(current.Values["label"]) != `"public"` {
		t.Fatalf("closed module owner changed durable settings: %+v, %v", current, err)
	}
}
