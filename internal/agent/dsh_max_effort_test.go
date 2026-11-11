package agent

import (
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
)

// TestDshMaxReachesOutputEffortModels: pi-ai asks a model on Messages for a
// budget unless it is told to think adaptively, and its budget path clamps
// max to high (16384), so DSH's Max on zcode's GLM-5.3 reached it as high
// (01huadalang on Discord, 10-24). GLM uses adaptive as pi-ai's effort
// carrier; the gateway sends enabled to the native endpoint. On the
// other routes, for MiniMax (not tried adaptive, #3496) and for a model
// that takes a budget, nothing changes.
func TestDshMaxReachesOutputEffortModels(t *testing.T) {
	for _, tc := range []struct {
		id    string
		route string
		want  bool
	}{
		{"zcode/GLM-5.3", "anthropic-messages", true},
		{"zai/glm-5.2", "anthropic-messages", true},
		{"minimax/MiniMax-M3.1-Flash", "anthropic-messages", false},
		{"anth/claude-sonnet-5", "anthropic-messages", true},
		{"kimi/kimi-k3", "anthropic-messages", false},
		{"zai/glm-4.6", "anthropic-messages", false},
		{"zcode/GLM-5.3", "openai-completions", false},
		{"zcode/GLM-5.3", "openai-responses", false},
	} {
		m := catalog.Model{ID: tc.id, Context: 1000000, Output: 128000, Efforts: []string{"low", "high", "max"}}
		for _, k := range dshKinds {
			if k.api != tc.route {
				continue
			}
			got := dshKindConfig(k, []catalog.Model{m}, tc.route, "http://127.0.0.1:3425").Models[0]
			if adaptive := got.Compat != nil && got.Compat.ForceAdaptiveThinking; adaptive != tc.want {
				t.Errorf("%s on %s: forceAdaptiveThinking %v, want %v", tc.id, tc.route, adaptive, tc.want)
			}
		}
	}
}

func TestDshMaxReachesAliasedGLM(t *testing.T) {
	dshRouteHome(t)
	p := provider.Provider{ID: "my-plan", Name: "Plan", Key: "k", Anthropic: "https://api.z.ai/api/anthropic", Models: []string{"fast"}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetUpstreamName("my-plan/fast", "GLM-5.3-Flash"); err != nil {
		t.Fatal(err)
	}
	m := catalog.Model{ID: "my-plan/fast", Efforts: []string{"low", "high", "max"}}
	got := dshKindConfig(dshKinds[2], []catalog.Model{m}, "anthropic-messages", "http://127.0.0.1:3425").Models[0]
	if got.Compat == nil || !got.Compat.ForceAdaptiveThinking {
		t.Fatalf("alias lost pi-ai max carrier: %+v", got.Compat)
	}
}
