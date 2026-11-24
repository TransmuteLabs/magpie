package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/autostart"
	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/routes"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSettingsVisionEffectiveModel(t *testing.T) {
	sandboxHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "1")
	const id, endpoint = "eye", "http://127.0.0.1:9/v1"
	const auto, manual = id + "/seer-mini", id + "/seer-manual"
	if err := provider.Save(provider.Provider{
		ID: id, Name: "Eye", Chat: endpoint, Key: "test-key", Models: []string{"seer-mini", "seer-manual"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(id, endpoint, []catalog.Model{
		{ID: "seer-mini", Efforts: []string{"high", "low"}},
		{ID: "seer-manual", Efforts: []string{"medium", "low"}},
	}); err != nil {
		t.Fatal(err)
	}
	held := filepath.Join(filepath.Dir(provider.Path()), "auto-picks.json")
	if err := os.WriteFile(held, []byte(`{"seeded":true,"vision":"eye"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	handler := Handler(nil, nil)
	for _, test := range []struct {
		name, picked, missing, effort string
		levels                        []string
	}{
		{name: "automatic", effort: "high", levels: []string{"low", "high"}},
		{name: "manual", picked: manual, effort: "medium", levels: []string{"low", "medium"}},
		{name: "missing", picked: "gone/eye", missing: "gone/eye", effort: "high", levels: []string{"low", "high"}},
		{name: "off", picked: "off"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := settings.Load()
			cfg.Vision, cfg.VisionEffort = test.picked, "xhigh"
			cfg.ModelImages = map[string]bool{auto: true, manual: true}
			if err := settings.Save(cfg); err != nil {
				t.Fatal(err)
			}
			read := func() {
				t.Helper()
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/settings", nil)
				reply := do(handler, req)
				var got settingsJSON
				if reply.Code != http.StatusOK || json.Unmarshal(reply.Body.Bytes(), &got) != nil {
					t.Fatalf("GET /api/settings: %d %s", reply.Code, reply.Body)
				}
				if got.Vision != test.picked || got.VisionAuto != auto || got.VisionMissing != test.missing ||
					got.VisionEffortNow != test.effort || !slices.Equal(got.VisionEfforts, test.levels) {
					t.Fatalf("vision = %q, auto %q, missing %q, levels %q, effort %q",
						got.Vision, got.VisionAuto, got.VisionMissing, got.VisionEfforts, got.VisionEffortNow)
				}
			}
			read()
			read()
		})
	}
}

func TestSettingsVisionFollowsForeignSDKCatalog(t *testing.T) {
	const module, vendor, model = "fixture.settings-vision-models", "vendor", "peer-fixture-vision-mini"
	const peer = "settings-vision-catalog-peer"
	id := provider.ExtensionProviderID(module, vendor)
	if slices.Contains(os.Args, peer) {
		providerAuthFixtureManager(t)
		selected, err := provider.Find(id)
		if err != nil || !selected.IsExtension() || !selected.Account.Lists(model) {
			t.Fatalf("peer could not select the SDK manifest model: %v", err)
		}
		models, err := selected.Fetch(t.Context())
		if err != nil || len(models) != 0 || selected.Account.Lists(model) {
			t.Fatalf("peer did not publish the confirmed empty catalog: models=%v, err=%v", models, err)
		}
		return
	}

	t.Run("before_request", func(t *testing.T) {
		testSettingsVisionCatalogWithdrawal(t, id, module, vendor, model, peer, false)
	})
	t.Run("during_response", func(t *testing.T) {
		testSettingsVisionCatalogWithdrawal(t, id, module, vendor, model, peer, true)
	})
}

func testSettingsVisionCatalogWithdrawal(t *testing.T, id, module, vendor, model, peer string, during bool) {
	t.Helper()
	home := sandboxHome(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	t.Setenv("MAGPIE_PLUGIN_MARKET", "off")
	manager := providerAuthFixtureManager(t)
	manifest := authfixture.Manifest()
	manifest.ID = module
	manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, sdk.OutboundExecutor)
	manifest.Providers[0].Models = []sdk.Model{{ID: model, Vision: true, Efforts: []string{"low", "high"}}}
	installProviderAuthFixture(t, manager, &manifest)
	if _, err := manager.Store().SaveAccount(extension.Account{
		Plugin: module, Provider: vendor, ID: "fixture-account", Auth: json.RawMessage(`{"token":"fixture"}`),
	}, 0); err != nil {
		t.Fatal(err)
	}
	cfg := settings.Load()
	cfg.VisionEffort = "high"
	if err := settings.Save(cfg); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	t.Cleanup(provider.Changed)

	handler := Handler(nil, nil)
	read := func() settingsJSON {
		t.Helper()
		reply := do(handler, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/settings", nil))
		var got settingsJSON
		if reply.Code != http.StatusOK || json.Unmarshal(reply.Body.Bytes(), &got) != nil {
			t.Fatalf("GET /api/settings: %d %s", reply.Code, reply.Body)
		}
		return got
	}
	full := id + "/" + model
	offersModel := func(models []modelRef) bool {
		return slices.ContainsFunc(models, func(item modelRef) bool { return item.ID == full })
	}
	withdrawn := func(got settingsJSON) bool {
		return got.VisionAuto == "" && !offersModel(got.VisionModels) && !offersModel(got.TitleModels) &&
			len(got.VisionEfforts) == 0 && got.VisionEffortNow == ""
	}
	for range 2 {
		got := read()
		if got.VisionAuto != full || !offersModel(got.VisionModels) || !offersModel(got.TitleModels) ||
			got.VisionEffortNow != "high" ||
			!slices.Equal(got.VisionEfforts, []string{"low", "high"}) {
			t.Fatalf("manifest model missing from Settings: auto=%q vision=%t title=%t levels=%v effort=%q",
				got.VisionAuto, offersModel(got.VisionModels), offersModel(got.TitleModels), got.VisionEfforts, got.VisionEffortNow)
		}
	}
	publish := func() {
		t.Helper()
		stamp := provider.StampNow()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
		defer cancel()
		writer := proc.CommandContext(ctx, executable,
			"-test.run=^TestSettingsVisionFollowsForeignSDKCatalog$", "-test.count=1", "--", peer,
		)
		if output, err := writer.CombinedOutput(); err != nil {
			t.Fatalf("publish models from another process: %v\n%s", err, output)
		}
		if provider.StampNow() != stamp {
			t.Fatal("peer catalog publication changed the ordinary provider stamp")
		}
	}
	started := false
	if during {
		previous := loginState
		t.Cleanup(func() { loginState = previous })
		loginState = func() autostart.State {
			if !started {
				started = true
				publish()
				if nested := read(); !withdrawn(nested) {
					t.Fatalf("overlapping Settings response still shows withdrawn catalog: auto=%q vision=%t title=%t",
						nested.VisionAuto, offersModel(nested.VisionModels), offersModel(nested.TitleModels))
				}
			}
			return autostart.Off
		}
	} else {
		publish()
	}

	got := read()
	if during && !started {
		t.Fatal("Settings did not reach the concurrent publication")
	}
	if !withdrawn(got) {
		t.Fatalf("Settings mixed SDK catalog revisions: auto=%q vision=%t title=%t levels=%v effort=%q",
			got.VisionAuto, offersModel(got.VisionModels), offersModel(got.TitleModels), got.VisionEfforts, got.VisionEffortNow)
	}
	if manager.Running(module) {
		t.Fatal("reading Settings started the SDK worker")
	}
}

func TestSettingsVisionFollowsForeignSDKModuleConfig(t *testing.T) {
	const module, vendor, model = "fixture.settings-vision-config", "vendor", "peer-config-vision-mini"
	const peer = "settings-vision-config-peer"
	if slices.Contains(os.Args, peer) {
		updated, err := providerAuthFixtureManager(t).CompareAndSwapModuleConfig(t.Context(), module, "0",
			json.RawMessage(`{"marker":"updated"}`))
		if err != nil || updated.Version != "1" {
			t.Fatalf("peer module configuration CAS: version=%q, err=%v", updated.Version, err)
		}
		return
	}

	home := sandboxHome(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	t.Setenv("MAGPIE_PLUGIN_MARKET", "off")
	manager := providerAuthFixtureManager(t)
	manifest := authfixture.Manifest()
	manifest.ID = module
	manifest.Contract.Capabilities = []sdk.Capability{sdk.ModuleRPC, sdk.AccountAuth, sdk.OutboundExecutor}
	manifest.Providers[0].Models = []sdk.Model{{ID: model, Vision: true, Efforts: []string{"low", "high"}}}
	installProviderAuthFixture(t, manager, &manifest)
	if _, err := manager.Store().SaveAccount(extension.Account{
		Plugin: module, Provider: vendor, ID: "fixture-account", Auth: json.RawMessage(`{"token":"fixture"}`),
	}, 0); err != nil {
		t.Fatal(err)
	}
	cfg := settings.Load()
	cfg.VisionEffort = "high"
	if err := settings.Save(cfg); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	t.Cleanup(provider.Changed)

	handler := Handler(nil, nil)
	read := func() settingsJSON {
		t.Helper()
		reply := do(handler, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/settings", nil))
		var got settingsJSON
		if reply.Code != http.StatusOK || json.Unmarshal(reply.Body.Bytes(), &got) != nil {
			t.Fatalf("GET /api/settings: %d %s", reply.Code, reply.Body)
		}
		return got
	}
	full := provider.ExtensionProviderID(module, vendor) + "/" + model
	offers := func(models []modelRef) bool {
		return slices.ContainsFunc(models, func(item modelRef) bool { return item.ID == full })
	}
	check := func(phase string, state settingsJSON) {
		t.Helper()
		if state.VisionAuto != full || !offers(state.VisionModels) || !offers(state.TitleModels) ||
			!slices.Equal(state.VisionEfforts, []string{"low", "high"}) || state.VisionEffortNow != "high" {
			t.Fatalf("%s: Settings SDK config projection auto=%q vision=%t title=%t levels=%v effort=%q",
				phase, state.VisionAuto, offers(state.VisionModels), offers(state.TitleModels), state.VisionEfforts, state.VisionEffortNow)
		}
	}
	check("warm", read())
	check("still warm", read())

	previous := loginState
	t.Cleanup(func() { loginState = previous })
	published := false
	loginState = func() autostart.State {
		t.Helper()
		loginState = previous
		before, err := extension.AccountStamp()
		if err != nil {
			t.Fatal(err)
		}
		stamp := provider.StampNow()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
		defer cancel()
		writer := proc.CommandContext(ctx, executable,
			"-test.run=^TestSettingsVisionFollowsForeignSDKModuleConfig$", "-test.count=1", "--", peer,
		)
		if output, err := writer.CombinedOutput(); err != nil {
			t.Fatalf("publish config from another process: %v\n%s", err, output)
		}
		after, err := extension.AccountStamp()
		if err != nil || after.Equal(before) || provider.StampNow() != stamp {
			t.Fatalf("peer CAS did not change only the unobserved SDK account source: %v", err)
		}
		published = true
		return autostart.Off
	}
	during := read()
	if !published || manager.Running(module) {
		t.Fatalf("expected only the SDK configuration writer, published=%t", published)
	}
	check("next request", read())
	check("overlapping config CAS", during)

	unrelated := filepath.Join(manager.Namespace(), "private", "unreadable-namespace")
	if err := os.WriteFile(unrelated, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := extension.AccountStamp(); err == nil {
		t.Fatal("fixture left SDK metadata readable")
	}
	check("unavailable account metadata", read())
	check("metadata still unavailable", read())
}

func TestSettingsVisionFollowsForeignSDKEnablement(t *testing.T) {
	const module, vendor, model = "fixture.settings-vision-enablement", "vendor", "peer-enabled-vision-mini"
	const strategyID, strategyName = "peer-order", "Peer order"
	home := sandboxHome(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	t.Setenv("MAGPIE_PLUGIN_MARKET", "off")
	manager := providerAuthFixtureManager(t)
	manifest := authfixture.Manifest()
	manifest.ID = module
	manifest.Contract.Capabilities = []sdk.Capability{sdk.ModuleRPC, sdk.AccountAuth, sdk.OutboundExecutor}
	manifest.Providers[0].Models = []sdk.Model{{ID: model, Vision: true, Efforts: []string{"low", "high"}}}
	manifest.Actions = []sdk.ActionDescriptor{{
		ID: authfixture.SettingsOrderAction, InputSchema: json.RawMessage(`{"type":"object"}`), Effect: "read",
	}}
	manifest.RoutingStrategies = []sdk.RoutingStrategyDescriptor{{
		ID: strategyID, Name: strategyName, Action: authfixture.SettingsOrderAction,
	}}
	installProviderAuthFixture(t, manager, &manifest)
	if _, err := manager.Store().SaveAccount(extension.Account{
		Plugin: module, Provider: vendor, ID: "fixture-account", Auth: json.RawMessage(`{"token":"fixture"}`),
	}, 0); err != nil {
		t.Fatal(err)
	}
	cfg := settings.Load()
	cfg.VisionEffort = "high"
	if err := settings.Save(cfg); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	t.Cleanup(provider.Changed)

	previousServer := served.Swap(nil)
	t.Cleanup(func() { served.Store(previousServer) })
	handler := Handler(nil, nil)
	read := func() settingsJSON {
		t.Helper()
		reply := do(handler, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/settings", nil))
		var got settingsJSON
		if reply.Code != http.StatusOK || json.Unmarshal(reply.Body.Bytes(), &got) != nil {
			t.Fatalf("GET /api/settings: %d %s", reply.Code, reply.Body)
		}
		return got
	}
	full := provider.ExtensionProviderID(module, vendor) + "/" + model
	wantRoute := routes.Option{ID: module + "/" + strategyID, Name: strategyName, Plugin: module}
	offers := func(models []modelRef) bool {
		return slices.ContainsFunc(models, func(item modelRef) bool { return item.ID == full })
	}
	check := func(phase string, got settingsJSON, enabled bool) {
		t.Helper()
		wantAuto, wantEffort := "", ""
		var wantLevels []string
		if enabled {
			wantAuto, wantLevels, wantEffort = full, []string{"low", "high"}, "high"
		}
		if phase == "overlapping publication" && !enabled && offers(got.VisionModels) && offers(got.TitleModels) &&
			(got.VisionAuto == full || got.VisionAuto == "") && len(got.VisionEfforts) == 0 && got.VisionEffortNow == "" {
			t.Fatal("overlapping SDK disable left the withdrawn model in both Settings model lists")
		}
		if got.VisionAuto != wantAuto || offers(got.VisionModels) != enabled || offers(got.TitleModels) != enabled ||
			!slices.Equal(got.VisionEfforts, wantLevels) || got.VisionEffortNow != wantEffort {
			t.Fatalf("%s (enabled=%t): Settings auto=%q vision=%t title=%t levels=%v effort=%q",
				phase, enabled, got.VisionAuto, offers(got.VisionModels), offers(got.TitleModels), got.VisionEfforts, got.VisionEffortNow)
		}
		routing := slices.Contains(got.RoutingOptions, wantRoute)
		compatibility := slices.Contains(got.JSRoutings, wantRoute)
		if routing != enabled || compatibility != enabled {
			t.Errorf("%s (enabled=%t): Settings routingOptions=%t jsRoutings=%t",
				phase, enabled, routing, compatibility)
		}
	}
	check("warm", read(), true)
	check("still warm", read(), true)

	filename := extension.CatalogPath()
	fileInfo := func() os.FileInfo {
		t.Helper()
		file, err := os.Open(filename)
		if err != nil {
			t.Fatal(err)
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil || closeErr != nil {
			t.Fatalf("stat catalog: %v, close: %v", statErr, closeErr)
		}
		return info
	}
	peer := extension.NewCatalog(manager.Namespace())
	previous := loginState
	t.Cleanup(func() { loginState = previous })
	for _, enabled := range []bool{false, true} {
		published := false
		loginState = func() autostart.State {
			loginState = previous
			before := fileInfo()
			stamp := provider.StampNow()
			if err := peer.SetEnabled(t.Context(), module, enabled); err != nil {
				t.Fatal(err)
			}
			if !enabled {
				body, err := os.ReadFile(filename)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, append(body, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chtimes(filename, before.ModTime(), before.ModTime()); err != nil {
				t.Fatal(err)
			}
			after := fileInfo()
			if os.SameFile(before, after) || before.Size() != after.Size() ||
				!before.ModTime().Equal(after.ModTime()) || provider.StampNow() != stamp {
				t.Fatal("expected a peer catalog replacement with the same time-and-size stamp")
			}
			published = true
			return autostart.Off
		}
		during := read()
		if !published || manager.Running(module) {
			t.Fatalf("expected the catalog writer without SDK worker activation, published=%t", published)
		}
		changed := fileInfo().ModTime().Add(-time.Hour)
		if err := os.Chtimes(filename, changed, changed); err != nil {
			t.Fatal(err)
		}
		check("next request", read(), enabled)
		check("overlapping publication", during, enabled)
	}
}
