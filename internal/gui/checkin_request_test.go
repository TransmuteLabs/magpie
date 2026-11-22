package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/protocolext"
)

func TestPluginReadsHonorRequestCancellation(t *testing.T) {
	sandboxHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	forgetLastState()
	t.Cleanup(forgetLastState)
	manager := extension.Default()
	source := t.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	manifest := sdk.Manifest{
		Schema: 1, ID: "checkin.fixture", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract: sdk.Contract{
			Version:      sdk.APIVersion{Major: 1, Minor: 8},
			Capabilities: []sdk.Capability{sdk.OutboundExecutor, sdk.AccountAuth, sdk.ModuleRPC},
		},
		Providers: []sdk.ProviderDescriptor{{
			ID: "vendor", Name: "SDK Fixture", Protocols: []protocolext.Protocol{"chat"},
			Models:  []sdk.Model{{ID: "fixture-model", Name: "Fixture Model"}},
			Checkin: &sdk.AccountCheckinDescriptor{Action: "daily.claim"},
		}},
		Actions: []sdk.ActionDescriptor{{
			ID: "daily.claim", Effect: "write", InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{sdk.ManifestFile: raw, entry: []byte("list reads must not execute the worker")}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(source, name), data, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := extension.NewCatalog(manager.Namespace()).Install(t.Context(), source, func(context.Context, extension.Installed) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Store().SaveAccount(extension.Account{
		Plugin: manifest.ID, Provider: "vendor", ID: "sdk-account", Auth: json.RawMessage(`{"token":"SDK-PRIVATE"}`),
	}, 0); err != nil {
		t.Fatal(err)
	}

	const spec, legacyID = "legacy-checkin-fixture", "legacyfixture"
	if err := os.WriteFile(plugin.ListPath(), []byte(`{"plugins":[{"spec":"`+spec+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plugin.AuthPath(), []byte(`{"legacyfixture":{"type":"oauth","accountId":"Legacy account","access":"LEGACY-PRIVATE"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	background := plugin.ListInBackground(false)
	t.Cleanup(func() {
		plugin.UseCached(nil)
		plugin.ListInBackground(background)
	})
	plugin.UseCached([]plugin.Provider{{
		ID: legacyID, Name: "Legacy Fixture", Spec: spec, Checkin: true,
		Methods: []plugin.Method{{Type: "oauth", Label: "Browser"}, {Type: "api", Label: "API key"}},
	}})

	sdkID := provider.ExtensionProviderID(manifest.ID, manifest.Providers[0].ID)
	for _, id := range []string{legacyID, sdkID} {
		if err := provider.Save(provider.Provider{ID: id, Off: true}); err != nil {
			t.Fatal(err)
		}
	}
	set := settings.Load()
	set.Currency = "usd"
	set.PluginCheckins = map[string]bool{sdkID: true}
	if err := settings.Save(set); err != nil {
		t.Fatal(err)
	}

	handler := Handler(nil, nil)
	for _, test := range []struct {
		name, method, path, body, lock string
	}{
		{"state catalog", http.MethodGet, "/api/state", "", "catalog.lock"},
		{"state store", http.MethodGet, "/api/state", "", filepath.Join("private", ".lock")},
		{"settings catalog", http.MethodGet, "/api/settings", "", "catalog.lock"},
		{"settings store", http.MethodGet, "/api/settings", "", filepath.Join("private", ".lock")},
		{"off catalog", http.MethodGet, "/api/usage/checkins/off", "", "catalog.lock"},
		{"off store", http.MethodGet, "/api/usage/checkins/off", "", filepath.Join("private", ".lock")},
		{"browser signin", http.MethodPost, "/api/plugin-signin", `{"provider":"legacyfixture","method":0}`, filepath.Join("private", ".lock")},
		{"key signin", http.MethodPost, "/api/plugin-signin", `{"provider":"legacyfixture","method":1,"key":"test"}`, filepath.Join("private", ".lock")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.method == http.MethodPost {
				t.Setenv("MAGPIE_NO_ACCOUNTS", "1")
			}
			provider.Changed()
			synctest.Test(t, func(t *testing.T) {
				file, err := os.OpenFile(filepath.Join(manager.Namespace(), test.lock), os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if held, err := filelock.TryLock(file); !held || err != nil {
					t.Fatal("hold SDK lock:", err)
				}
				release := sync.OnceFunc(func() { filelock.Unlock(file) })
				defer func() {
					release()
					if test.path == "/api/state" {
						deadline := time.Now().Add(10 * time.Second)
						for provider.FetchingNew() && time.Now().Before(deadline) {
							time.Sleep(5 * time.Millisecond)
						}
						if provider.FetchingNew() {
							t.Error("the state's background model check did not exit")
						}
					}
				}()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				req := httptest.NewRequestWithContext(ctx, test.method, test.path, strings.NewReader(test.body))
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() { done <- do(handler, req) }()
				synctest.Wait()
				select {
				case response := <-done:
					t.Fatalf("returned before cancellation with the lock held: %d %s", response.Code, response.Body)
				default:
				}
				cancel()
				synctest.Wait()
				select {
				case response := <-done:
					if response.Body.Len() != 0 {
						t.Errorf("canceled caller received a reply: %s", response.Body)
					}
				default:
					release()
					<-done
					t.Errorf("%s waited for %s after its caller canceled", test.path, test.lock)
				}
			})
		})
	}

	stateReply := do(handler, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	var state stateJSON
	if stateReply.Code != http.StatusOK || json.Unmarshal(stateReply.Body.Bytes(), &state) != nil || !state.Settings.PluginCheckins[sdkID] {
		t.Fatalf("state did not recover with its saved check-in switch: %d %s", stateReply.Code, stateReply.Body)
	}
	settingsReply := do(handler, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	var view settingsJSON
	if settingsReply.Code != http.StatusOK || json.Unmarshal(settingsReply.Body.Bytes(), &view) != nil {
		t.Fatalf("settings did not recover: %d %s", settingsReply.Code, settingsReply.Body)
	}
	for _, id := range []string{legacyID, sdkID} {
		if index := slices.IndexFunc(view.CheckinPlugins, func(row provider.PluginCheckin) bool { return row.ID == id }); index < 0 || view.CheckinPlugins[index].On != (id == sdkID) {
			t.Fatalf("settings lost check-in provider or switch %q: %+v", id, view.CheckinPlugins)
		}
		if !slices.ContainsFunc(view.CheckinAccounts, func(row provider.CheckinAccount) bool { return row.By == "plugin:"+id && row.On }) {
			t.Fatalf("settings lost check-in account %q: %+v", id, view.CheckinAccounts)
		}
	}
	cardsReply := do(handler, httptest.NewRequest(http.MethodGet, "/api/usage/checkins/off", nil))
	var cards []provider.SubscriptionQuota
	if cardsReply.Code != http.StatusOK || json.Unmarshal(cardsReply.Body.Bytes(), &cards) != nil || len(cards) != 2 {
		t.Fatalf("switched-off cards did not recover: %d %s", cardsReply.Code, cardsReply.Body)
	}
	for _, id := range []string{legacyID, sdkID} {
		if !slices.ContainsFunc(cards, func(row provider.SubscriptionQuota) bool {
			return row.Provider == id && row.CheckinBy == "plugin:"+id && row.Checkins && row.CheckinKey != "" && row.Windows != nil && len(row.Windows) == 0
		}) {
			t.Fatalf("switched-off card missing for %q: %+v", id, cards)
		}
	}
	if manager.Running(manifest.ID) {
		t.Fatal("a check-in metadata read started the SDK worker")
	}
}
