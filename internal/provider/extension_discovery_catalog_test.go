package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func TestExtensionDiscoveryFencesPeerChanges(t *testing.T) {
	manager := discoveryCatalogManager(t)
	peer := extensionhost.NewManager(manager.Namespace())
	t.Cleanup(func() { closeDiscoveryCatalogManager(t, peer) })
	source, entry := buildDispatchBindingWorker(t)
	const first, other = "fixture.discovery-first", "fixture.discovery-other"
	installDiscoveryCatalogModule(t, manager, source, entry, first, 9)
	installDiscoveryCatalogModule(t, manager, source, entry, other, 8)
	firstID, otherID := ExtensionProviderID(first, first+"-vendor"), ExtensionProviderID(other, other+"-vendor")
	discover := func() Provider {
		t.Helper()
		providers, err := ExtensionProviders()
		if err != nil || len(providers) != 2 {
			t.Fatalf("discover installed providers: got %d, error %v", len(providers), err)
		}
		index := slices.IndexFunc(providers, func(provider Provider) bool { return provider.ID == firstID })
		if index < 0 || !providers[index].Account.Lists("m1") || providers[index].Account.Lists("absent") {
			t.Fatal("first provider or its declared model unavailable")
		}
		return providers[index]
	}
	ask := func(selected Provider, config string) {
		t.Helper()
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, selected.Anthropic+"/messages",
			strings.NewReader(`{"model":"m1"}`))
		response, err := selected.Do(http.DefaultClient, request)
		if response != nil {
			defer response.Body.Close()
		}
		if config == "" {
			if err == nil || response != nil || selected.Account.Lists("m1") {
				t.Fatalf("stale provider remained available: response %v, error %v", response, err)
			}
			return
		}
		if err != nil || response == nil || response.StatusCode != http.StatusForbidden {
			t.Fatalf("current provider did not reach fixture: response %v, error %v", response, err)
		}
		body, err := io.ReadAll(response.Body)
		if err != nil || string(body) != "1.0.0/"+config {
			t.Fatalf("worker used wrong configuration: %q, %v", body, err)
		}
	}
	firstSelection := discover()
	if _, err := peer.CompareAndSwapModuleConfig(t.Context(), first, "0", json.RawMessage(`{"route":"next"}`)); err != nil {
		t.Fatal(err)
	}
	ask(firstSelection, "")
	configured := discover()

	if err := peer.SetEnabled(t.Context(), first, false); err != nil {
		t.Fatal(err)
	}
	unavailable := func() {
		t.Helper()
		providers, err := ExtensionProviders()
		if err != nil || len(providers) != 1 || providers[0].ID != otherID || !providers[0].Account.Lists("m1") {
			t.Fatalf("discovery lost the healthy sibling: %v, %v", providers, err)
		}
	}
	unavailable()
	if err := peer.SetEnabled(t.Context(), first, true); err != nil {
		t.Fatal(err)
	}
	ask(configured, "")
	ask(discover(), "1")

	if _, err := peer.Store().StateCASContext(t.Context(), sdk.Scope{Plugin: first}, "module.config", 1,
		json.RawMessage(`{"route":"a","route":"b"}`)); err != nil {
		t.Fatal(err)
	}
	unavailable()
}

func TestExtensionDiscoveryCancelsWhileConfigurationStoreIsLocked(t *testing.T) {
	manager := discoveryCatalogManager(t)
	source, entry := buildDispatchBindingWorker(t)
	const id = "fixture.discovery-config"
	installDiscoveryCatalogModule(t, manager, source, entry, id, 8)
	if _, err := manager.CompareAndSwapModuleConfig(t.Context(), id, "0", json.RawMessage(`{"route":"saved"}`)); err != nil {
		t.Fatal(err)
	}
	unlock, held := filelock.Lock(filepath.Join(manager.Namespace(), "private", ".lock"), 0)
	if !held {
		t.Fatal("could not hold private store")
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	providers, logins, err := providerExtensionsContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || providers != nil || logins != nil {
		t.Fatalf("canceled discovery returned %d providers, %d logins, error %v", len(providers), len(logins), err)
	}
}

func discoveryCatalogManager(t *testing.T) *extensionhost.Manager {
	t.Helper()
	testenv.SetHome(t, t.TempDir())
	manager := extensionhost.NewManager(t.TempDir())
	previous := providerExtensionHost
	providerExtensionHost = func() extensionProviderHost { return manager }
	t.Cleanup(func() {
		providerExtensionHost = previous
		closeDiscoveryCatalogManager(t, manager)
	})
	return manager
}

func closeDiscoveryCatalogManager(t *testing.T, manager *extensionhost.Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Error(err)
	}
}

func installDiscoveryCatalogModule(t *testing.T, manager *extensionhost.Manager, source, entry, id string, minor int) {
	t.Helper()
	manifest := sdk.Manifest{
		Schema: sdk.ManifestSchema, ID: id, Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: minor},
			Capabilities: []sdk.Capability{sdk.OutboundExecutor}},
		Providers: []sdk.ProviderDescriptor{{
			ID: id + "-vendor", Name: "Fixture", Protocols: []protocolext.Protocol{protocolext.Messages},
			Models: []sdk.Model{{ID: "m1", Name: "Fixture model"}},
		}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(t.Context(), source); err != nil {
		t.Fatal(err)
	}
}
