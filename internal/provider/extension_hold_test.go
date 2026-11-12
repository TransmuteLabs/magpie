package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/plugin"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func TestExtensionProviderCatalogChangesInvalidateHeldSnapshots(t *testing.T) {
	providerFileHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "1")
	plugin.UseCached(nil)
	oldHeldFor, oldKeptFor := heldFor, keptFor
	heldFor, keptFor = time.Hour, time.Hour
	t.Cleanup(func() { heldFor, keptFor = oldHeldFor, oldKeptFor })

	root := filepath.Dir(extensionhost.CatalogPath())
	manager := extensionhost.NewManager(root)
	before := providerExtensionHost
	providerExtensionHost = func() extensionProviderHost { return manager }
	t.Cleanup(func() {
		providerExtensionHost = before
		Changed()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Errorf("close extension manager: %v", err)
		}
	})
	manifest := sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: "fixture.catalog", Version: "1.0.0", Runtime: "exec", Entry: "worker",
		Contract: sdk.Contract{Version: sdk.FullContract().Version, Capabilities: []sdk.Capability{sdk.OutboundExecutor}},
		Providers: []sdk.ProviderDescriptor{{ID: "public", Name: "Catalog fixture", Protocols: []protocolext.Protocol{"chat"},
			Models: []sdk.Model{{ID: "m1", Protocols: []protocolext.Protocol{"chat"}}}}},
	}
	id := ExtensionProviderID(manifest.ID, manifest.Providers[0].ID)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, manifest.Entry), []byte("fixture, never executed"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Catalog writes bypass Manager's in-process callbacks, as a separate
	// CLI does. The handshake is stubbed; no worker needs to run here.
	foreign := extensionhost.NewCatalog(root)
	install := func() {
		t.Helper()
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := foreign.Install(t.Context(), source, func(ctx context.Context, _ extensionhost.Installed) error {
			return ctx.Err()
		}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func(want ...string) Stamp {
		t.Helper()
		defer Hold()()
		found := slices.ContainsFunc(All(), func(provider Provider) bool { return provider.ID == id })
		if found != (len(want) != 0) {
			t.Fatalf("provider snapshot: present=%v want models=%v", found, want)
		}
		var models []string
		for _, entry := range Catalog() {
			if strings.HasPrefix(entry.ID, id+"/") {
				models = append(models, strings.TrimPrefix(entry.ID, id+"/"))
			}
		}
		slices.Sort(models)
		if !slices.Equal(models, want) {
			t.Fatalf("model catalog snapshot: got=%v want=%v", models, want)
		}
		return StampNow()
	}
	stamp := snapshot()
	if snapshot() != stamp {
		t.Fatal("an unchanged catalog moved its stamp")
	}
	for _, step := range []struct {
		name string
		run  func() error
		want []string
	}{
		{"install", func() error { install(); return nil }, []string{"m1"}},
		{"disable", func() error { return foreign.SetEnabled(t.Context(), manifest.ID, false) }, nil},
		{"enable", func() error { return foreign.SetEnabled(t.Context(), manifest.ID, true) }, []string{"m1"}},
		{"upgrade", func() error {
			manifest.Version = "1.0.1"
			manifest.Providers[0].Models = []sdk.Model{{ID: "updated-model", Protocols: []protocolext.Protocol{"chat"}}}
			install()
			return nil
		}, []string{"updated-model"}},
	} {
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		next := snapshot(step.want...)
		if next == stamp {
			t.Fatalf("%s: another process's catalog write left GUI/model stamps unchanged", step.name)
		}
		stamp = next
		if snapshot(step.want...) != stamp {
			t.Fatalf("%s: an unchanged catalog moved its stamp", step.name)
		}
	}
	func() {
		defer Hold()()
		snapshot("updated-model")
		if err := manager.SetEnabled(t.Context(), manifest.ID, false); err != nil {
			t.Fatal(err)
		}
		snapshot()
		if err := manager.SetEnabled(t.Context(), manifest.ID, true); err != nil {
			t.Fatal(err)
		}
		snapshot("updated-model")
	}()
	stamp = snapshot("updated-model")
	if err := foreign.Remove(t.Context(), manifest.ID); err != nil {
		t.Fatal(err)
	}
	if snapshot() == stamp {
		t.Fatal("another process's removal left GUI/model stamps unchanged")
	}
}
