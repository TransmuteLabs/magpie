package extension

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

func TestAcquireRetiresWorkerAfterExternalChange(t *testing.T) {
	for _, change := range []string{"disabled", "removed", "reenabled", "configuration", "package"} {
		t.Run(change, func(t *testing.T) {
			manager, installed := fixtureManager(t)
			ctx := t.Context()
			id := installed.Manifest.ID
			old, err := manager.Acquire(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			defer old.Close()
			expected := &expectedGeneration{
				packageRevision:  old.Revision(),
				configVersion:    old.ConfigVersion(),
				enableGeneration: &old.worker.entry.EnableGeneration,
			}
			peer := NewManager(manager.Namespace())
			switch change {
			case "disabled", "reenabled":
				if err = peer.catalog.SetEnabled(ctx, id, false); err == nil && change == "reenabled" {
					err = peer.catalog.SetEnabled(ctx, id, true)
				}
			case "removed":
				err = peer.catalog.Remove(ctx, id)
			case "configuration":
				_, err = peer.CompareAndSwapModuleConfig(ctx, id, expected.configVersion, json.RawMessage(`{"peer":true}`))
			case "package":
				err = peer.Restart(ctx, id)
			}
			if err != nil {
				t.Fatal(err)
			}

			if rejected, err := manager.acquireLease(ctx, id, expected); err == nil {
				rejected.Close()
				t.Fatal("the old generation was admitted")
			}
			if manager.Running(id) {
				t.Fatal("a rejected acquisition left the superseded worker cached")
			}
			old.worker.mu.Lock()
			draining := old.worker.retiring && old.worker.active == 1
			old.worker.mu.Unlock()
			if !draining || !old.client.alive() {
				t.Fatal("the old lease lost its worker before draining")
			}

			if change != "disabled" && change != "removed" {
				current, err := manager.Acquire(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				defer current.Close()
				if current.worker == old.worker {
					t.Fatal("a new acquisition reused the retired worker")
				}
				if rejected, err := manager.acquireLease(ctx, id, expected); err == nil {
					rejected.Close()
					t.Fatal("the stale expectation was admitted into the current worker")
				}
				same, err := manager.Acquire(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				defer same.Close()
				if same.worker != current.worker || same.client != current.client {
					t.Fatal("the stale expectation displaced the current worker")
				}
			}

			old.Close()
			select {
			case <-old.client.done:
			case <-time.After(5 * time.Second):
				t.Fatal("the retired worker did not stop after the old lease closed")
			}
		})
	}
}

func TestAcquireCancellationKeepsWorkerDuringCatalogContention(t *testing.T) {
	manager, installed := fixtureManager(t)
	id := installed.Manifest.ID
	old, err := manager.Acquire(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	peer := NewCatalog(manager.Namespace())
	if err := peer.SetEnabled(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	unlock, held := filelock.Lock(filepath.Join(peer.Root, "catalog.lock"), 0)
	if !held {
		t.Fatal("could not hold the peer catalog lock")
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if rejected, err := manager.Acquire(ctx, id); !errors.Is(err, context.DeadlineExceeded) {
		if rejected != nil {
			rejected.Close()
		}
		t.Fatalf("acquisition did not return the caller's deadline: %v", err)
	}
	if !manager.Running(id) {
		t.Fatal("a canceled read withdrew a cached worker")
	}
}
