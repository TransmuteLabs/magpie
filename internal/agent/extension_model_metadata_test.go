package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func agentModelMetadataFixture(t *testing.T, minor int) (*Agent, string) {
	t.Helper()
	descriptor := sdk.AgentDescriptor{ID: "fixture-model-metadata", Name: "Model metadata fixture", Bin: "fixture-model-editor",
		ConfigFiles: []string{".model-fixture/settings.json"}, Fields: []sdk.AgentField{{Key: "model", Label: "Model"}},
		Launch: &sdk.AgentLaunch{}}
	installAgentWorkerFixture(t, descriptor, "./testdata/modelmetadataworker", minor)
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".model-fixture", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"userNote":{"keep":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "metadata", Name: "Metadata", Chat: "http://127.0.0.1:1/v1",
		Key: "fixture", Models: []string{"wide", "unknown", "empty"}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("metadata", "http://127.0.0.1:1/v1", []catalog.Model{
		{ID: "wide", Context: 400000, Output: 500000, Efforts: []string{"low", "medium", "high"}, DefaultEffort: "medium"},
		{ID: "unknown"},
		{ID: "empty", Efforts: []string{}},
	}); err != nil {
		t.Fatal(err)
	}
	a, err := Find(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a, path
}

func readAgentModelMetadata(t *testing.T, path string) map[string]map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Models   []map[string]json.RawMessage `json:"models"`
		UserNote map[string]bool              `json:"userNote"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil || !settings.UserNote["keep"] {
		t.Fatalf("ordinary action lost user configuration: %s (%v)", raw, err)
	}
	models := map[string]map[string]json.RawMessage{}
	for _, model := range settings.Models {
		var value string
		if err := json.Unmarshal(model["value"], &value); err != nil {
			t.Fatal(err)
		}
		models[value] = model
	}
	return models
}

func requireAgentModelFact(t *testing.T, models map[string]map[string]json.RawMessage, model, field, want string) {
	t.Helper()
	if got := string(models[model][field]); got != want {
		t.Fatalf("ordinary SDK agent model %s lost effective %s: got %q, want %q", model, field, got, want)
	}
}

func TestExtensionAgentModelMetadataOrdinaryLifecycle(t *testing.T) {
	a, path := agentModelMetadataFixture(t, 9)
	if _, err := a.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	models := readAgentModelMetadata(t, path)
	requireAgentModelFact(t, models, "magpie/metadata/wide", "maxOutput", "400000")
	requireAgentModelFact(t, models, "magpie/metadata/wide", "efforts", `["low","medium","high"]`)
	requireAgentModelFact(t, models, "magpie/metadata/wide", "defaultEffort", `"medium"`)
	requireAgentModelFact(t, models, "magpie/metadata/unknown", "efforts", "null")
	requireAgentModelFact(t, models, "magpie/metadata/unknown", "maxOutput", "")
	requireAgentModelFact(t, models, "magpie/metadata/empty", "efforts", "[]")
	if !a.Wired() {
		t.Fatal("ordinary controlled inspection failed after model metadata connect")
	}
	if err := provider.SetModelOutput("metadata/wide", 16384); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetModelEfforts("metadata/wide", []string{"low", "high"}); err != nil {
		t.Fatal(err)
	}
	SyncCatalog()
	models = readAgentModelMetadata(t, path)
	requireAgentModelFact(t, models, "magpie/metadata/wide", "maxOutput", "16384")
	requireAgentModelFact(t, models, "magpie/metadata/wide", "efforts", `["low","high"]`)
	requireAgentModelFact(t, models, "magpie/metadata/wide", "defaultEffort", "")
	if err := provider.SetModelReasonless("metadata/wide", true); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply("model", "magpie/metadata/wide"); err != nil {
		t.Fatal(err)
	}
	models = readAgentModelMetadata(t, path)
	requireAgentModelFact(t, models, "magpie/metadata/wide", "efforts", "[]")
	requireAgentModelFact(t, models, "magpie/metadata/wide", "defaultEffort", "")
}

func TestExtensionAgentModelMetadataLegacyOff(t *testing.T) {
	a, path := agentModelMetadataFixture(t, 8)
	if _, err := a.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	for model, facts := range readAgentModelMetadata(t, path) {
		for _, field := range []string{"maxOutput", "efforts", "defaultEffort"} {
			if _, exists := facts[field]; exists {
				t.Fatalf("SDK 1.8 model %s borrowed API 1.9 metadata %s", model, field)
			}
		}
	}
}

func TestAgentModelMetadataRoutingGroupUsesEffectiveEntry(t *testing.T) {
	a, path := agentModelMetadataFixture(t, 9)
	if err := provider.SaveGroup(provider.Group{Name: "metadata", Members: []string{"metadata/wide"}, Levels: []string{"low", "high"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	shown, _ := provider.CatalogFor(a.ID)
	for _, entry := range shown {
		if entry.ID == "group/metadata" {
			models := readAgentModelMetadata(t, path)
			requireAgentModelFact(t, models, "magpie/group/metadata", "context", "400000")
			requireAgentModelFact(t, models, "magpie/group/metadata", "maxOutput", "400000")
			requireAgentModelFact(t, models, "magpie/group/metadata", "efforts", `["low","high"]`)
			requireAgentModelFact(t, models, "magpie/group/metadata", "defaultEffort", "")
			return
		}
	}
	t.Fatal("ordinary provider catalog omitted fixture routing group")
}

func TestAgentModelMetadataCallerCannotMutateCache(t *testing.T) {
	a, _ := agentModelMetadataFixture(t, 9)
	options := viaMagpie(a.ID, "magpie/")
	index := slices.IndexFunc(options, func(option Option) bool { return option.Value == "magpie/metadata/wide" })
	if index < 0 || len(options[index].Efforts) != 3 {
		t.Fatal("ordinary options lost effective model effort levels")
	}
	options[index].Efforts[0] = "changed"
	fresh := viaMagpie(a.ID, "magpie/")
	if fresh[index].Efforts[0] != "low" {
		t.Fatal("a caller changed the cached model effort authority")
	}
	first := Option{Value: "m", Efforts: nil}
	second := Option{Value: "m", Efforts: []string{}}
	if sameOption(first, second) {
		t.Fatal("option equality collapsed unknown and explicit empty effort support")
	}
}
