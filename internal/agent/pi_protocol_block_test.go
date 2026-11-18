package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
)

func piProtocolBlockFixture(t *testing.T) (*Agent, string) {
	t.Helper()
	home := syncHome(t)
	for _, entry := range []provider.Provider{
		{ID: "responses", Name: "Responses", Key: "synthetic", Responses: "http://127.0.0.1:1/v1", Models: []string{"fixture-response"}},
		{ID: "messages", Name: "Messages", Key: "synthetic", Anthropic: "http://127.0.0.1:1", Models: []string{"fixture-message"}},
	} {
		if err := provider.Save(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := pi(home).Field("model").Set("magpie/relay/glm-4.6"); err != nil {
		t.Fatal(err)
	}
	setUpInMagpie("pi")
	ordinary, err := Find("pi")
	if err != nil {
		t.Fatal(err)
	}
	return ordinary, filepath.Join(home, ".pi", "agent", "models.json")
}

func piProtocolBlockRead(t *testing.T, path string) map[string]any {
	t.Helper()
	var document struct {
		Providers map[string]map[string]any `json:"providers"`
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	return document.Providers["magpie"]
}

func TestPiProtocolPickRewritesExistingBlockWithoutAPI(t *testing.T) {
	for _, method := range []string{"Set", "Apply"} {
		t.Run(method, func(t *testing.T) {
			ordinary, path := piProtocolBlockFixture(t)
			if err := edit.SetJSON(path, edit.KV{Path: "providers.personal", Value: map[string]any{"api": "personal-api", "baseUrl": "http://127.0.0.1:1/personal"}}); err != nil {
				t.Fatal(err)
			}
			if err := edit.DelJSON(path, "providers.magpie.api"); err != nil {
				t.Fatal(err)
			}
			for _, protocol := range []string{"openai-responses", "anthropic-messages"} {
				if method == "Set" {
					if err := ordinary.Field("protocol").Set(protocol); err != nil {
						t.Fatal(err)
					}
				} else if err := ordinary.Apply("protocol", protocol); err != nil {
					t.Fatal(err)
				}
				block := piProtocolBlockRead(t, path)
				if block["api"] != protocol || block["baseUrl"] != gatewayV1() {
					t.Fatalf("successful %s left existing provider unchanged before Sync: api=%v baseUrl=%v, want %s at %s", method, block["api"], block["baseUrl"], protocol, gatewayV1())
				}
				seen := map[string]bool{}
				for _, raw := range block["models"].([]any) {
					model := raw.(map[string]any)
					seen[model["id"].(string)] = true
					if protocol == "openai-responses" {
						if model["api"] != nil || model["baseUrl"] != nil {
							t.Fatalf("immediate Responses pick kept model override: %+v", model)
						}
					} else if model["api"] != protocol || model["baseUrl"] != gateway.URL() {
						t.Fatalf("immediate Messages pick lost root/API: %+v", model)
					}
				}
				for _, id := range []string{"relay/glm-4.6", "responses/fixture-response", "messages/fixture-message"} {
					if !seen[id] {
						t.Fatalf("immediate pick lost model %s", id)
					}
				}
				if personal, _ := edit.GetJSON(path, "providers.personal.api"); personal != "personal-api" {
					t.Fatal("protocol selection changed another provider")
				}
			}
		})
	}
}
