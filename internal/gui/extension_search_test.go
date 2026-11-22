package gui

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSDKSearchSettingsUsesOrdinarySelection(test *testing.T) {
	home := test.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME"} {
		test.Setenv(name, filepath.Join(home, name))
	}
	test.Setenv("MAGPIE_BUN", filepath.Join(home, "no-bun"))
	test.Setenv("MAGPIE_NO_ACCOUNTS", "1")
	directory := test.TempDir()
	entry := "worker"
	if runtime.GOOS == "windows" {
		entry += ".exe"
	}
	manifest := sdk.Manifest{Schema: sdk.ManifestSchema, ID: "search.fixture", Version: "1.0.0", Runtime: "exec", Entry: entry,
		Contract:      sdk.Contract{Version: sdk.APIVersion{Major: 1, Minor: 8}, Capabilities: []sdk.Capability{sdk.ModuleRPC}},
		Actions:       []sdk.ActionDescriptor{{ID: "search-read", Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		SearchSources: []sdk.SearchSourceDescriptor{{ID: "specstream", Name: "SpecStream fixture", Action: "search-read"}}}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, sdk.ManifestFile), raw, 0600); err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(test.Context(), 2*time.Minute)
	defer cancel()
	command := proc.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-o",
		filepath.Join(directory, entry), "../extension/testdata/searchworker")
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		test.Fatalf("build Settings search fixture: %v\n%s", err, output)
	}
	manager := extension.Default()
	test.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			test.Error(err)
		}
	})
	if _, err := manager.Install(test.Context(), directory); err != nil {
		test.Fatal(err)
	}
	const vendor = "sdk:search.fixture:specstream"
	var state settingsJSON
	searchState(&state, provider.All(), provider.Served())
	index := slices.IndexFunc(state.SearchVendors, func(value searchVendorJSON) bool { return value.ID == vendor })
	if index < 0 || state.SearchVendors[index].NeedURL || !state.SearchVendors[index].KeyOptional || manager.Running("search.fixture") {
		test.Fatal("ordinary Settings did not offer an SDK source without starting its runtime")
	}
	handler := Handler(nil, nil)
	post := func(body string) {
		test.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("POST", "/api/settings/search-api", strings.NewReader(body)))
		if response.Code != 200 {
			test.Fatalf("settings: %d %s", response.Code, response.Body)
		}
	}
	post(`{"vendor":"` + vendor + `"}`)
	if saved := provider.SearchAPIs(); len(saved) != 1 || saved[0].Vendor != vendor {
		test.Fatalf("Settings did not select source in the ordinary search owner: %+v", saved)
	}
	post(`{"vendor":"` + vendor + `","on":false}`)
	searchState(&state, provider.All(), provider.Served())
	if len(state.SearchAPIs) != 1 || !state.SearchAPIs[0].Off || len(provider.SearchAPIs()) != 0 {
		test.Fatal("Settings Off did not suppress source dispatch")
	}
	post(`{"vendor":"` + vendor + `","on":true}`)
	if err := manager.SetEnabled(test.Context(), "search.fixture", false); err != nil {
		test.Fatal(err)
	}
	searchState(&state, provider.All(), provider.Served())
	if slices.ContainsFunc(state.SearchVendors, func(value searchVendorJSON) bool { return value.ID == vendor }) ||
		len(state.SearchAPIs) != 1 || state.SearchAPIs[0].Ready || manager.Running("search.fixture") {
		test.Fatal("disabled module remained an available Settings source")
	}
}
