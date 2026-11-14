package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSDKSearchCrossManagerPublicationFence(test *testing.T) {
	directory := buildSDKSearch(test)
	for _, mutation := range []string{"disable", "config", "replacement"} {
		test.Run(mutation, func(test *testing.T) {
			manager := setupSDKSearch(test, directory, false)
			other := extension.NewManager(manager.Namespace())
			test.Cleanup(func() { closeSDKGatewayHook(test, other) })
			started := filepath.Join(test.TempDir(), "started")
			release := filepath.Join(test.TempDir(), "release")
			configureSDKSearch(test, manager, map[string]any{"mode": "hold", "started": started, "release": release})
			binding, found := manager.SearchSource("search.fixture/specstream")
			if !found {
				test.Fatal("source not discovered")
			}
			ctx, cancel := context.WithTimeout(test.Context(), 15*time.Second)
			finished := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, err := manager.RunSearch(ctx, binding, sdk.Scope{Plugin: "search.fixture"},
					sdk.SearchRequest{Source: "specstream", Query: "held", Count: 2})
				finished <- err
			}()
			test.Cleanup(func() {
				cancel()
				_ = os.WriteFile(release, []byte("release"), 0600)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					test.Error("cross-manager search outlived cleanup")
				}
			})
			waitSDKSearchMarker(test, started)
			switch mutation {
			case "disable":
				if err := other.SetEnabled(test.Context(), "search.fixture", false); err != nil {
					test.Fatal(err)
				}
			case "config":
				configureSDKSearch(test, other, map[string]any{})
			case "replacement":
				installSDKSearch(test, other, directory, "2.0.0", false)
			}
			if !manager.Running("search.fixture") {
				test.Fatal("peer mutation stopped the fixture before publication was tested")
			}
			if err := os.WriteFile(release, []byte("release"), 0600); err != nil {
				test.Fatal(err)
			}
			select {
			case err := <-finished:
				if err == nil {
					test.Fatalf("held search published after another manager's %s", mutation)
				}
			case <-time.After(10 * time.Second):
				test.Fatal("cross-manager search did not finish")
			}
		})
	}
}
