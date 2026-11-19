package agent

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestOpenCodeDiscoveryUsesAdoptedSDKCatalog(t *testing.T) {
	// Production can initialize usage's built-in UA table before the profile
	// is installed. Discovery must still recognize the profile's exact ID.
	if got := usage.AgentOf("opencode/2.0"); got != "opencode" {
		t.Fatalf("built-in OpenCode user agent = %q", got)
	}
	descriptor := launchAgentDescriptor()
	descriptor.ID = "opencode-sdk"
	descriptor.Profile = &sdk.AgentProfile{Target: "opencode"}
	descriptor.ConfigFiles = []string{".fixture/config.json"}
	host, _ := launchAgentFixture(t, descriptor)

	for _, fixture := range []struct {
		id     string
		models []string
	}{
		{"base", []string{"plain"}},
		{"shared", []string{"common"}},
		{"profile", []string{"one", "two", "hidden"}},
	} {
		if err := provider.Save(provider.Provider{ID: fixture.id, Name: fixture.id, Key: "fake",
			Chat: "http://127.0.0.1:1/v1", Models: fixture.models}); err != nil {
			t.Fatal(err)
		}
	}
	prefs := settings.Load()
	prefs.Visible = map[string][]string{"opencode": {"base", "shared"}, descriptor.ID: {"profile", "shared"}}
	prefs.HiddenModels = map[string][]string{descriptor.ID: {"profile/hidden"}}
	prefs.OrderedModels = map[string][]string{"opencode": {"base/plain", "shared/common"},
		descriptor.ID: {"profile/two", "shared/common", "profile/one"}}
	prefs.AgentContexts = map[string]map[string]int{"opencode": {"shared/common": 65536},
		descriptor.ID: {"shared/common": 131072}}
	if err := settings.Save(prefs); err != nil {
		t.Fatal(err)
	}
	secret, err := access.Update("add-key", access.Change{Name: "Held"})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := access.List()
	if err != nil || len(keys) != 1 {
		t.Fatalf("fixture gateway keys: %v (%d)", err, len(keys))
	}
	if _, err := access.Update("models-key", access.Change{Key: keys[0].ID, Models: []string{"profile/one", "shared/common", "profile/hidden"}}); err != nil {
		t.Fatal(err)
	}

	handler := gateway.New().Handler()
	check := func(phase, token, userAgent string, want []string, window int) {
		t.Helper()
		for _, format := range []string{gateway.OpenCodeFormat, gateway.OpenCode2Format} {
			t.Run(phase+"/"+format, func(t *testing.T) {
				request := httptest.NewRequest("GET", "/v1/models?format="+format, nil)
				request.RemoteAddr = "127.0.0.1:15891"
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("User-Agent", userAgent)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("catalog response: %d %s", response.Code, response.Body)
				}
				var catalog struct {
					Models []struct {
						ID    string
						Entry struct {
							Limit struct{ Context int }
						}
					}
				}
				if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil {
					t.Fatal(err)
				}
				var ids []string
				for _, model := range catalog.Models {
					ids = append(ids, model.ID)
					if model.ID == "shared/common" && model.Entry.Limit.Context != window {
						t.Errorf("shared/common context = %d, want %d", model.Entry.Limit.Context, window)
					}
				}
				if !slices.Equal(ids, want) {
					t.Errorf("catalog models = %v, want %v", ids, want)
				}
			})
		}
	}
	builtin := []string{"base/plain", "shared/common"}
	profileToken := gateway.TokenFor(descriptor.ID)
	check("before-adoption", profileToken, "", builtin, 65536)
	profile, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	check("active-profile", profileToken, "", []string{"profile/two", "shared/common", "profile/one"}, 131072)
	check("restricted-profile", secret, descriptor.ID+"/2.0", []string{"shared/common", "profile/one"}, 131072)
	check("builtin-token", gateway.TokenFor("opencode"), "", builtin, 65536)
	check("unidentified-client", gateway.Token, "curl/8", builtin, 65536)
	config, err := host.ModuleConfig(t.Context(), "fixture.agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.CompareAndSwapModuleConfig(t.Context(), "fixture.agent", config.Version, json.RawMessage(`{"changed":true}`)); err != nil {
		t.Fatal(err)
	}
	check("stale-profile-config", profileToken, "", builtin, 65536)
	check("stale-restricted-key", secret, descriptor.ID, []string{"shared/common"}, 65536)
	if err := profile.Sync(); err != nil {
		t.Fatal(err)
	}
	check("resynced-profile", profileToken, "", []string{"profile/two", "shared/common", "profile/one"}, 131072)
	if err := host.SetEnabled(t.Context(), "fixture.agent", false); err != nil {
		t.Fatal(err)
	}
	check("disabled-profile", profileToken, "", builtin, 65536)
}
