package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/gateway"
)

func holdAgentConfig(t *testing.T, path, key, before, after, oldValue string) func() {
	t.Helper()
	writeFile(t, path, before)
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	release := filememo.Hold()
	t.Cleanup(release)
	if got, _ := edit.GetJSON(path, key); got != oldValue {
		t.Fatalf("fixture %s = %q, want %q", key, got, oldValue)
	}
	writeFile(t, path, after)
	if got, _ := edit.GetJSON(path, key); got != oldValue {
		t.Fatalf("fixture did not hold previous %s: %q", key, got)
	}
	return release
}

func TestPiCatalogSyncKeepsCurrentCompatibility(t *testing.T) {
	agent := pi(syncHome(t))
	models := filepath.Join(agent.Dir, "models.json")
	writeFile(t, agent.Path, `{"defaultProvider":"magpie","defaultModel":"relay/glm-4.6"}`)
	before := `{"providers":{"magpie":{"compat":{"sendSessionAffinityHeaders":true},"models":[]}}}`
	after := strings.Replace(before, "true", "false", 1)
	const key = "providers.magpie.compat.sendSessionAffinityHeaders"
	release := holdAgentConfig(t, models, key, before, after, "true")
	if err := agent.Sync(); err != nil {
		t.Fatal(err)
	}
	release()
	if value, _ := edit.GetJSON(models, key); value != "false" {
		t.Errorf("Pi Sync replaced the current affinity setting with %q", value)
	}
	if catalog, _ := edit.GetJSON(models, "providers.magpie.models"); !strings.Contains(catalog, "relay/glm-4.6") {
		t.Errorf("Pi Sync did not refresh the catalog: %s", catalog)
	}
}

func TestPiCatalogSyncRollsBackWhenScopeChanges(t *testing.T) {
	agent := pi(syncHome(t))
	models := filepath.Join(agent.Dir, "models.json")
	const before = `{"defaultProvider":"magpie","defaultModel":"relay/glm-4.6","enabledModels":["magpie/relay/old","magpie/relay/glm-4.6"]}`
	const catalog = `{"providers":{"magpie":{"models":[{"id":"relay/old"},{"id":"relay/glm-4.6"}]}}}`
	const user = `{"defaultProvider":"magpie","defaultModel":"relay/glm-4.6","enabledModels":["magpie/relay/old","magpie/relay/glm-4.6"],"theme":"dark"}`
	writeFile(t, agent.Path, before)
	writeFile(t, models, catalog)
	target, err := edit.Target(agent.Path)
	if err != nil {
		t.Fatal(err)
	}
	edited := false
	err = edit.Filtered(func(path string, body []byte) []byte {
		if path == target && !edited {
			edited = true
			writeFile(t, agent.Path, user)
		}
		return body
	}, agent.Sync)
	if !edited || !errors.Is(err, extension.ErrActionChanged) {
		t.Fatalf("Pi Sync accepted a changed scope: edited=%t, err=%v", edited, err)
	}
	if readFile(agent.Path) != user || readFile(models) != catalog {
		t.Error("Pi Sync left a partial catalog update or lost the user's settings")
	}
	if stashShared()[apisWantedKey(models)] != "" || stashShared()[outputsWantedKey(models)] != "" {
		t.Error("Pi Sync published catalog history for a rejected plan")
	}
	if err := agent.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(agent.Path); strings.Contains(got, "relay/old") || !strings.Contains(got, `"theme":"dark"`) {
		t.Errorf("Pi Sync did not reconcile the current scope: %s", got)
	}
}

func TestAsideRefreshUsesCurrentProviderOwner(t *testing.T) {
	for _, operation := range []string{"sync", "connect"} {
		t.Run(operation, func(t *testing.T) {
			_, models := asideHome(t)
			agent := mustFindAside(t)
			key := gateway.TokenFor("aside")
			before := `{"providers":{"magpie":{"apiKey":"` + key + `","baseUrl":"` + gatewayV1() + `","api":"openai-completions","models":[]}}}`
			after := strings.Replace(before, key, "foreign-key", 1)
			release := holdAgentConfig(t, models, "providers.magpie.apiKey", before, after, key)
			call := agent.Sync
			if operation == "connect" {
				call = agent.Connect
			}
			err := call()
			release()
			if operation == "connect" && err == nil {
				t.Error("Connect accepted the foreign provider")
			}
			if operation == "sync" && err != nil {
				t.Errorf("Sync did not skip the foreign provider: %v", err)
			}
			if got := readFile(models); got != after {
				t.Errorf("%s overwrote the foreign provider: %s", operation, got)
			}
		})
	}
}

func TestZedCatalogSyncUsesCurrentProvider(t *testing.T) {
	for _, change := range []string{"effort", "address"} {
		t.Run(change, func(t *testing.T) {
			agent := zedAt(filepath.Join(syncHome(t), "zed"))
			before := `{"language_models":{"openai_compatible":{"magpie":{"api_url":"` + gatewayV1() +
				`","available_models":[{"name":"relay/glm-4.6","reasoning_effort":"high"}]}}}}`
			key, oldValue, newValue := zedProvider+".available_models.0.reasoning_effort", "high", "none"
			if change == "address" {
				key, oldValue, newValue = zedProvider+".api_url", gatewayV1(), "https://example.invalid/v1"
			}
			after := strings.Replace(before, oldValue, newValue, 1)
			release := holdAgentConfig(t, agent.Path, key, before, after, oldValue)
			if err := agent.Sync(); err != nil {
				t.Fatal(err)
			}
			release()
			if change == "address" {
				if got := readFile(agent.Path); got != after {
					t.Errorf("Zed Sync edited a foreign provider: %s", got)
				}
			} else if got, _ := edit.GetJSON(agent.Path, key); got != newValue {
				t.Errorf("Zed Sync restored the old effort: %q", got)
			}
		})
	}
}

func TestQoderCatalogSyncUsesCurrentModelAndProvider(t *testing.T) {
	agent := qoder(syncHome(t))
	before := `{"model":{"name":"magpie/relay/old"},"providers":{"magpie":{"displayName":"before","routing":{"summary":"before"},"models":[]}}}`
	after := `{"model":{"name":"magpie/relay/glm-4.6"},"providers":{"magpie":{"displayName":"after","routing":{"summary":"after"},"models":[]}}}`
	release := holdAgentConfig(t, agent.Path, "model.name", before, after, "magpie/relay/old")
	if err := agent.Sync(); err != nil {
		t.Fatal(err)
	}
	release()
	for key, expected := range map[string]string{
		"providers.magpie.model":           "relay/glm-4.6",
		"providers.magpie.displayName":     "after",
		"providers.magpie.routing.summary": "after",
	} {
		if got, _ := edit.GetJSON(agent.Path, key); got != expected {
			t.Errorf("Qoder Sync set %s to %q, want %q", key, got, expected)
		}
	}
}
