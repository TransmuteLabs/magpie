package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
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
