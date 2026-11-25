package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func TestSettingsProjectionsDeferAutoPickSettlement(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("MAGPIE_NO_ACCOUNTS", "1")

	const endpoint = "https://api.openai.com/v1"
	if err := provider.Save(provider.Provider{
		ID: "oai", Name: "OpenAI", Responses: endpoint, Chat: endpoint, Key: "sk-test",
		Models: []string{"gpt-image-2", "sora-2", "gpt-5.4-mini"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("oai", endpoint, []catalog.Model{
		{ID: "gpt-image-2", OutputModalities: []string{"image"}},
		{ID: "sora-2", Films: true, OutputModalities: []string{"video"}},
		{ID: "gpt-5.4-mini", Efforts: []string{"low", "high"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(settings.Settings{Currency: "usd", SearcherEffort: "high"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(autoPicksPath(), []byte(`{"seeded":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	providers := provider.All()
	var drawers SettingsDrawers
	drawerProj := drawers.Project(providers, "")
	videoProj := ProjectVideoSettingsFor(providers, "")
	visionProj := ProjectVisionSettingsFor(providers, "", "high")
	searchProj := ProjectSearchSettings(providers, "", "high")

	if drawerProj.Auto != "oai/gpt-image-2" {
		t.Fatalf("drawerProj.Auto = %q, want oai/gpt-image-2", drawerProj.Auto)
	}
	if videoProj.Auto != "oai/sora-2" {
		t.Fatalf("videoProj.Auto = %q, want oai/sora-2", videoProj.Auto)
	}
	if visionProj.Missing != "" {
		t.Fatalf("visionProj.Missing = %q, want empty", visionProj.Missing)
	}
	if !strings.HasPrefix(searchProj.Auto, "OpenAI") || !strings.HasPrefix(searchProj.Searcher, "OpenAI") {
		t.Fatalf("searchProj = %+v, want OpenAI auto/searcher", searchProj)
	}
	if len(searchProj.Choices) == 0 {
		t.Fatalf("searchProj.Choices empty")
	}
	if DrawerHiddenIn(providers, "oai/gpt-image-2") {
		t.Fatalf("DrawerHiddenIn reported visible model hidden")
	}

	before := heldFile(t)
	if before.Drawer != "" || before.Video != "" || before.Searcher != "" {
		t.Fatalf("Project* wrote auto-picks before Settle(): %+v", before)
	}

	drawerProj.Settle()
	videoProj.Settle()
	visionProj.Settle()
	searchProj.Settle()

	after := heldFile(t)
	if after.Drawer != "oai" || after.Video != "oai" || after.Searcher != "oai" {
		t.Fatalf("Settle() did not persist expected holds: %+v", after)
	}
}
