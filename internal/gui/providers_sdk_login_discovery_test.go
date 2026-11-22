package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestProvidersPageSDKLoginDiscovery(t *testing.T) {
	for _, count := range []int{1, 4, 10} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			sandboxHome(t)
			t.Setenv("MAGPIE_NO_ACCOUNTS", "")
			manager := providerAuthFixtureManager(t)
			registry := extension.NewCatalog(manager.Namespace())
			publicIDs := make([]string, 0, count)
			moduleIDs := make([]string, 0, count)
			providerIDs := make([]string, 0, count)
			for index := range count {
				manifest := authfixture.Manifest()
				manifest.ID = fmt.Sprintf("fixture.page-logins-%d", index)
				manifest.Window, manifest.AuthBindings = nil, nil
				manifest.Contract.Version.Minor = 9
				manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.OutboundExecutor)
				providerID := fmt.Sprintf("fixture-vendor-%d", index)
				manifest.Providers[0].ID = providerID
				manifest.Providers[0].Models = []sdk.Model{{ID: "fixture-model", Name: "Fixture Model"}}
				manifest.Providers[0].Options = []sdk.ProviderOption{{Key: "paid", Label: "Paid", Default: true}}
				if runtime.GOOS == "windows" {
					manifest.Entry += ".exe"
				}
				source := t.TempDir()
				body, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(source, sdk.ManifestFile), body, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(source, manifest.Entry), []byte("static model inventory"), 0o700); err != nil {
					t.Fatal(err)
				}
				if _, err := registry.Install(t.Context(), source, func(context.Context, extension.Installed) error { return nil }); err != nil {
					t.Fatal(err)
				}
				for _, user := range []string{"alice", "bob"} {
					if _, err := manager.Store().SaveAccount(extension.Account{
						Plugin: manifest.ID, Provider: providerID, ID: user, Name: user + "@example.test",
						Auth: json.RawMessage(`{"token":"fixture-private"}`),
					}, 0); err != nil {
						t.Fatal(err)
					}
				}
				publicID := provider.ExtensionProviderID(manifest.ID, providerID)
				if err := catalog.SaveLive(publicID, "", []catalog.Model{{ID: "fixture-model"}}); err != nil {
					t.Fatal(err)
				}
				if err := provider.SetLoginOn(publicID, "bob", false); err != nil {
					t.Fatal(err)
				}
				publicIDs = append(publicIDs, publicID)
				moduleIDs = append(moduleIDs, manifest.ID)
				providerIDs = append(providerIDs, providerID)
			}
			if err := provider.Save(provider.Provider{
				ID: "fixture-native", Name: "Independent provider", Chat: "http://127.0.0.1:1/v1",
				Key: "fixture-key", Models: []string{"independent-model"},
			}); err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			providerRoutes(mux, nil)
			handler := held(mux)
			provider.Changed()

			check := func(phase string, removed bool) {
				t.Helper()
				t.Log("P06 providers begin", count, phase)
				state := readProviderSignInState(t, handler)
				t.Log("P06 providers end", count, phase)
				rows := make(map[string]providerSignInRow, len(state.Providers))
				for _, row := range state.Providers {
					rows[row.ID] = row
				}
				if rows["fixture-native"].Name != "Independent provider" {
					t.Fatal("unrelated configured provider is missing")
				}
				for index, publicID := range publicIDs {
					row := rows[publicID]
					if row.Account == nil || row.Account.Agent != publicID {
						t.Fatalf("%s: missing SDK account projection", publicID)
					}
					options := row.ExtensionOptions
					if options == nil || len(options.Options) != 1 || options.Options[0].Key != "paid" || !options.Options[0].On {
						t.Fatalf("%s: missing provider options: %+v", publicID, options)
					}
					logins := row.Account.Logins
					want := 2
					if removed && index == 0 {
						want = 1
					}
					if len(logins) != want || logins[0].Agent != publicID || logins[0].User != "alice" ||
						!logins[0].Active || !logins[0].On || logins[0].ExtensionGeneration != "1" ||
						row.AccountUsers["alice"] != "alice@example.test" {
						t.Fatalf("%s: unexpected active and backup accounts: %+v, aliases %v", publicID, logins, row.AccountUsers)
					}
					if want == 2 && (logins[1].Agent != publicID || logins[1].User != "bob" ||
						logins[1].On || logins[1].Active || row.AccountUsers["bob"] != "bob@example.test") {
						t.Fatalf("%s: unexpected disabled backup account: %+v, aliases %v", publicID, logins[1], row.AccountUsers)
					}
					if want == 1 && row.AccountUsers["bob"] != "" {
						t.Fatalf("%s: removed account's alias remains visible", publicID)
					}
				}
				for _, moduleID := range moduleIDs {
					if manager.Running(moduleID) {
						t.Fatalf("displaying logins started %s", moduleID)
					}
				}
			}
			check("cold", false)
			check("warm", false)
			peer := extension.NewManager(manager.Namespace())
			t.Cleanup(func() { _ = peer.Close(context.Background()) })
			if err := peer.Store().DeleteAccount(moduleIDs[0], providerIDs[0], "bob", 1); err != nil {
				t.Fatal(err)
			}
			check("after_peer_delete", true)

			t.Setenv("MAGPIE_NO_ACCOUNTS", "1")
			state := readProviderSignInState(t, handler)
			var native bool
			for _, row := range state.Providers {
				if row.ID == "fixture-native" {
					native = true
				}
				for _, publicID := range publicIDs {
					if row.ID == publicID {
						t.Fatalf("%s: private SDK row survived disabling discovery", publicID)
					}
				}
			}
			if !native {
				t.Fatal("disabling discovery hid the independent provider")
			}
		})
	}
}
