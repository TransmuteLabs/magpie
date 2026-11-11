package provider

import (
	"testing"

	"github.com/yetone/magpie/internal/catalog"
)

func TestZhipuProtocolCodingPlanPreference(t *testing.T) {
	azureHome(t)
	for _, tc := range []struct {
		name, preset, chat, responses, anthropic, model string
		want                                            Protocol
	}{
		{"custom Z.ai", "", "https://api.z.ai/api/coding/paas/v4", "", "https://api.z.ai/api/anthropic", "GLM-5.3", Anthropic},
		{"custom BigModel", "", "https://open.bigmodel.cn/api/coding/paas/v4", "", "https://open.bigmodel.cn/api/anthropic", "glm-5.2", Anthropic},
		{"Z.ai responses only", "", "", "https://api.z.ai/api/v1", "https://api.z.ai/api/anthropic", "glm-5.3", Anthropic},
		{"BigModel responses only", "", "", "https://open.bigmodel.cn/api/v1", "https://open.bigmodel.cn/api/anthropic", "glm-5.2", Anthropic},
		{"preset mirror", "zai", "https://mirror.invalid/api/coding/paas/v4", "", "https://mirror.invalid/api/anthropic", "glm-5.1", Chat},
		{"coding chat to relay", "", "https://api.z.ai/api/coding/paas/v4", "", "https://relay.invalid/api/anthropic", "glm-5.3", Chat},
		{"coding chat to other family", "", "https://api.z.ai/api/coding/paas/v4", "", "https://open.bigmodel.cn/api/anthropic", "glm-5.3", Chat},
		{"coding chat to wrong path", "", "https://api.z.ai/api/coding/paas/v4", "", "https://api.z.ai/relay", "glm-5.3", Chat},
		{"responses to other family", "", "", "https://open.bigmodel.cn/api/v1", "https://api.z.ai/api/anthropic", "glm-5.3", Responses},
		{"responses relay", "", "", "https://relay.invalid/api/v1", "https://relay.invalid/api/anthropic", "glm-5.3", Responses},
		{"payg", "zai", "https://api.z.ai/api/paas/v4", "", "https://api.z.ai/api/anthropic", "glm-5.3", Chat},
		{"third party", "", "https://relay.invalid/api/coding/paas/v4", "", "https://relay.invalid/api/anthropic", "glm-5.3", Chat},
		{"another model", "", "https://api.z.ai/api/coding/paas/v4", "", "https://api.z.ai/api/anthropic", "other", Chat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Provider{ID: "custom", Preset: tc.preset, Key: "k", Chat: tc.chat, Responses: tc.responses, Anthropic: tc.anthropic}
			if got := p.Native(tc.model); got != tc.want {
				t.Errorf("Native(%q) = %q, want %q", tc.model, got, tc.want)
			}
			if got := p.OnMessages(tc.model); got != (tc.want == Anthropic) {
				t.Errorf("OnMessages(%q) = %v", tc.model, got)
			}
		})
	}
	mirror := Provider{Preset: "zai", Key: "k", Chat: "https://mirror.invalid/api/coding/paas/v4"}
	if got := ZhipuCodingSearch(mirror); got != "https://mirror.invalid"+ZhipuSearchPath {
		t.Errorf("preset mirror search = %q", got)
	}
	p, err := FromPreset("zai")
	if err != nil {
		t.Fatal(err)
	}
	p.ID, p.Name, p.Key = "plans", "Plans", "payg-key"
	plan := p.WithKey(KeyAccount{Key: "plan-key", Plan: "coding"})
	if plan.Native("glm-5.3") != Anthropic || !plan.OnMessages("glm-5.3") || p.Native("glm-5.3") != Chat {
		t.Errorf("per-key plan did not retain its own preference: payg %s, coding %s", p.Native("glm-5.3"), plan.Native("glm-5.3"))
	}
	onlyMessages := p.WithKey(KeyAccount{Key: "plan-key", Plan: "coding", Protocol: Anthropic})
	if !onlyMessages.OnMessages("glm-5.3") {
		t.Error("Messages-only plan key lost its official family")
	}
}

func TestZhipuProtocolAliasesAndAPIOverrides(t *testing.T) {
	azureHome(t)
	p := Provider{ID: "plan", Name: "Plan", Key: "k", Chat: "https://api.z.ai/api/coding/paas/v4", Responses: "https://api.z.ai/api/v1", Anthropic: "https://api.z.ai/api/anthropic", Models: []string{"alias", "glm-listed", "glm-pinned", "glm-5.2"}}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(p.ID, p.Chat, []catalog.Model{{ID: "alias"}, {ID: "glm-listed", APIs: []string{"chat", "anthropic"}}, {ID: "glm-pinned"}, {ID: "glm-5.2"}}); err != nil {
		t.Fatal(err)
	}
	if err := SetUpstreamName("plan/alias", "GLM-5.3-Flash"); err != nil {
		t.Fatal(err)
	}
	if err := SetUpstreamName("plan/glm-listed", "GLM-5.3"); err != nil {
		t.Fatal(err)
	}
	if err := SetUpstreamName("plan/glm-pinned", "GLM-5.3"); err != nil {
		t.Fatal(err)
	}
	if err := SetModelAPI("plan/glm-pinned", "responses"); err != nil {
		t.Fatal(err)
	}
	q, err := Find("plan")
	if err != nil {
		t.Fatal(err)
	}
	if q.Native("alias") != Anthropic || !q.OnMessages("alias") {
		t.Error("wire alias did not prefer Messages")
	}
	if q.Native("glm-listed") != Chat || q.OnMessages("glm-listed") {
		t.Error("listed APIs did not retain their routing")
	}
	if q.Native("glm-pinned") != Responses || q.OnMessages("glm-pinned") {
		t.Error("explicit Responses choice was not respected")
	}
	if _, changed, err := SetModelID("plan/glm-5.2", "friendly"); err != nil || !changed {
		t.Fatalf("SetModelID: %v %v", changed, err)
	}
	if q.Native("friendly") != Anthropic || !q.OnMessages("friendly") {
		t.Error("local model id lost the GLM wire identity")
	}
}
