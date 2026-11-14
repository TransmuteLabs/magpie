package provider

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/plugin"
)

func TestStartupKeepsUninstalledSubscriptionsForExplicitMigration(t *testing.T) {
	for _, state := range []string{"", MoveMoving, MoveFailed, MovedBack, MovePlugin, MoveRemoved} {
		t.Run("state="+state, func(t *testing.T) {
			claudeHome(t)
			fakeMover(t)
			movers["fakeco"].min = "0.2.0"
			priorRetiring := retiring
			retiring = func() []string { return []string{"fakeco"} }
			t.Cleanup(func() { retiring = priorRetiring })
			var installs atomic.Int32
			installPlugin = func(context.Context, string, string) error {
				installs.Add(1)
				return errors.New("unrequested plugin install")
			}
			loginsMu.Lock()
			err := writeLogins([]savedLogin{fakeLogin("saved@example.test", "preserved-token", true, true)})
			loginsMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if state != "" {
				if err := setMigration("fakeco", func(m *Migration) {
					*m = Migration{
						State: state, Package: "fake", At: time.Unix(0, 0),
						Backup: []savedLogin{fakeLogin("saved@example.test", "preserved-token", true, true)},
					}
				}); err != nil {
					t.Fatal(err)
				}
			}
			beforeLogins, err := os.ReadFile(loginsPath())
			if err != nil {
				t.Fatal(err)
			}
			beforeMigration, migrationErr := os.ReadFile(migrationsPath())
			if migrationErr != nil && !errors.Is(migrationErr, os.ErrNotExist) {
				t.Fatal(migrationErr)
			}
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() {
					defer close(done)
					KeepRetiringMoved(ctx)
				}()
				time.Sleep(21 * time.Second)
				cancel()
				<-done
			})
			if installs.Load() != 0 {
				t.Fatalf("startup attempted %d unrequested external plugin installs for migration state %q", installs.Load(), state)
			}
			if entries := plugin.Load().Plugins; len(entries) != 0 {
				t.Fatalf("startup populated the empty plugin catalog: %+v", entries)
			}
			afterLogins, err := os.ReadFile(loginsPath())
			if err != nil {
				t.Fatal(err)
			}
			if state == MovePlugin {
				if slices.ContainsFunc(readLogins(), func(login savedLogin) bool { return login.Agent == "fakeco" }) {
					t.Fatal("startup left already-moved credentials in the builtin login store")
				}
				migration, ok := MigrationOf("fakeco")
				if !ok || len(migration.Backup) != 1 || fakeToken(migration.Backup[0]) != "preserved-token" {
					t.Fatal("startup lost the backup of an already-moved account")
				}
				return
			}
			if !bytes.Equal(beforeLogins, afterLogins) {
				t.Fatal("startup changed unmigrated account bytes without an installed plugin")
			}
			afterMigration, err := os.ReadFile(migrationsPath())
			matchingRead := err == nil && migrationErr == nil || errors.Is(err, os.ErrNotExist) && errors.Is(migrationErr, os.ErrNotExist)
			if !matchingRead || !bytes.Equal(beforeMigration, afterMigration) {
				t.Fatalf("startup changed an uninstalled migration record: before error %v, after error %v", migrationErr, err)
			}
		})
	}
}

func TestStartupMaintainsOnlyInstalledMigrationTargets(t *testing.T) {
	for _, state := range []string{"", MoveMoving, MoveFailed, MovedBack} {
		t.Run("state="+state, func(t *testing.T) {
			ctx, installed, installs := fakePlugin(t)
			priorRetiring := retiring
			retiring = func() []string { return []string{"fakeco"} }
			t.Cleanup(func() { retiring = priorRetiring })
			if _, err := plugin.Add(ctx, installed); err != nil {
				t.Fatal(err)
			}
			setFakeLogins(t, fakeLogin("saved@example.test", "preserved-token", true, true))
			if state != "" {
				if err := setMigration("fakeco", func(m *Migration) {
					*m = Migration{State: state, Package: installed, At: time.Unix(0, 0)}
				}); err != nil {
					t.Fatal(err)
				}
			}
			result := moveRetiring(ctx, false)
			if len(result) != 1 || result["fakeco"] != nil || !Moved("fakeco") || *installs != 1 {
				t.Fatalf("installed migration did not finish: results %+v, moved %v, install checks %d", result, Moved("fakeco"), *installs)
			}
			if len(fakeSaved(t)) != 0 || !plugin.SignedIn("fakeco") {
				t.Fatal("installed migration did not retain its account through the plugin")
			}
		})
	}
	t.Run("disabled", func(t *testing.T) {
		ctx, installed, installs := fakePlugin(t)
		priorRetiring := retiring
		retiring = func() []string { return []string{"fakeco"} }
		t.Cleanup(func() { retiring = priorRetiring })
		if _, err := plugin.Add(ctx, installed); err != nil {
			t.Fatal(err)
		}
		if err := SetPluginOff(ctx, installed, true); err != nil {
			t.Fatal(err)
		}
		setFakeLogins(t, fakeLogin("saved@example.test", "preserved-token", true, true))
		before, err := os.ReadFile(loginsPath())
		if err != nil {
			t.Fatal(err)
		}
		result := moveRetiring(ctx, false)
		after, err := os.ReadFile(loginsPath())
		if err != nil {
			t.Fatal(err)
		}
		if len(result) != 0 || *installs != 0 || !bytes.Equal(before, after) {
			t.Fatalf("startup changed a disabled migration target: results %+v, installs %d", result, *installs)
		}
		if _, ok := MigrationOf("fakeco"); ok {
			t.Fatal("startup created a migration for a disabled plugin")
		}
	})
}
