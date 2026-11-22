package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestActiveModuleConfigFencesEnablementAndConfiguration(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := componentFixtureManifest()
	manifest.Contract.Version.Minor = 8
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.LedgerObserver)
	manifest.Ledger = &sdk.LedgerObservationDescriptor{}
	installed := installComponentFixture(t, manager, manifest)
	id := manifest.ID
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), id, "0", json.RawMessage(`{"label":"first"}`)); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		unlock, held := filelock.Lock(filepath.Join(manager.Namespace(), "private", ".lock"), 0)
		if !held {
			t.Fatal("could not hold the private store")
		}
		released := false
		defer func() {
			if !released {
				unlock()
			}
		}()
		type result struct {
			entry  Installed
			config sdk.ConfigSnapshot
			err    error
		}
		done := make(chan result, 1)
		go func() {
			entry, config, err := manager.ActiveModuleConfig(t.Context(), id)
			done <- result{entry, config, err}
		}()
		synctest.Wait()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		if err := manager.catalog.SetEnabled(ctx, id, false); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("enablement write passed a pending configuration selection: %v", err)
		}
		unlock()
		released = true
		got := <-done
		if got.err != nil || got.entry.Revision != installed.Revision || got.entry.EnableGeneration != "" ||
			got.config.Version != "1" || string(got.config.Values) != `{"label":"first"}` {
			t.Fatalf("initial enabled snapshot: %s %q %+v %v", got.entry.Revision, got.entry.EnableGeneration, got.config, got.err)
		}
	})
	if err := manager.catalog.SetEnabled(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.ActiveModuleConfig(t.Context(), id); err == nil {
		t.Fatal("disabled module was selected")
	}
	if config, err := manager.ModuleConfig(t.Context(), id); err != nil || config.Version != "1" {
		t.Fatalf("ordinary configuration read lost the disabled module: %+v %v", config, err)
	}
	if err := manager.catalog.SetEnabled(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	if entry, config, err := manager.ActiveModuleConfig(t.Context(), id); err != nil ||
		entry.Revision != installed.Revision || entry.EnableGeneration == "" || config.Version != "1" {
		t.Fatalf("re-enabled snapshot: %s %q %+v %v", entry.Revision, entry.EnableGeneration, config, err)
	}
	if err := os.WriteFile(filepath.Join(manager.Namespace(), observerEnablementFile(id)), []byte(`"broken"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.ActiveModuleConfig(t.Context(), id); err == nil {
		t.Fatal("module with unreadable enablement was selected")
	}
}
