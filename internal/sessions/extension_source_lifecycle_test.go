package sessions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSourceNativeRevocationDuringPrepare(t *testing.T) {
	for _, change := range []string{"consent", "off", "remove", "config", "store"} {
		t.Run(change, func(t *testing.T) {
			manager, installed, path := installedSourceFixture(t)
			before, ok := Find("fixture-source", "sess")
			if !ok {
				t.Fatal("initial source missing")
			}
			sourceFixtureWrite(t, path, "new bytes", 23)
			started, release := make(chan struct{}), make(chan struct{})
			open := sync.OnceFunc(func() { close(release) })
			sourceBeforeRPC = func(ctx context.Context) error {
				close(started)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			finished := make(chan error, 1)
			stopped := make(chan struct{})
			go func() { defer close(stopped); finished <- PrepareSessionSources(ctx) }()
			t.Cleanup(func() { cancel(); open(); <-stopped; sourceBeforeRPC = nil })
			select {
			case <-started:
			case err := <-finished:
				t.Fatal("source preparation did not reach barrier", err)
			case <-time.After(10 * time.Second):
				t.Fatal("source preparation did not start")
			}
			switch change {
			case "consent":
				if err := SetReads(nil, map[string]bool{"fixture-source": false}); err != nil {
					t.Fatal(err)
				}
			case "off":
				if err := manager.SetEnabled(t.Context(), installed.Manifest.ID, false); err != nil {
					t.Fatal(err)
				}
			case "remove":
				if err := manager.Remove(t.Context(), installed.Manifest.ID); err != nil {
					t.Fatal(err)
				}
			case "config":
				config, err := manager.ModuleConfig(t.Context(), installed.Manifest.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := manager.CompareAndSwapModuleConfig(t.Context(), installed.Manifest.ID, config.Version, json.RawMessage(`{"changed":true}`)); err != nil {
					t.Fatal(err)
				}
			case "store":
				root := filepath.Dir(path)
				if err := os.Rename(root, root+".previous"); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			open()
			if err := <-finished; err == nil {
				t.Fatal("changed authority/store published prepared result")
			}
			<-stopped
			sourceBeforeRPC = nil
			if _, err := TranscriptOf(before); err == nil {
				t.Fatal("old source retained transcript authority")
			}
			if _, err := CarryOver(before, "omp"); err == nil {
				t.Fatal("old source retained conversion authority")
			}
			_, visible := Find("fixture-source", "sess")
			if visible != (change == "config") {
				t.Fatal("revocation/recovery visibility is wrong", change, visible)
			}
		})
	}
}
