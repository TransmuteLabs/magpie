package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	fixture "github.com/yetone/magpie/internal/plugin/testdata/nativeaccount"
)

// This uses the embedded host, real account hooks and a loopback-only vendor;
// it does not install an external provider or contact a registry.
func TestZCodeEmbeddedHostSignInStartWireAndObservations(t *testing.T) {
	claudeHome(t)
	vendor := fixture.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if len(plugin.Load().Plugins) != 0 {
		t.Fatal("test requires an empty external plugin list")
	}
	signed, err := plugin.APIKey(ctx, "zcode", 6, map[string]string{"site": "zai"}, "fixture-plan-key", plugin.NewAccount)
	if err != nil || signed.Provider != "zcode" || signed.Account == "" {
		t.Fatalf("native key sign-in = %+v, %v", signed, err)
	}
	startState, _ := json.Marshal(map[string]any{"site": "zai", "kind": "start", "uid": "start-user", "zcodeUserId": "start-user",
		"device": "11111111-2222-4333-8444-555555555555", "base": "https://api.z.ai/api/anthropic", "jwt": "fixture-start-jwt"})
	start, err := plugin.Import(ctx, "zcode", map[string]any{"type": "oauth", "refresh": string(startState), "access": "fixture-start-jwt", "accountId": "start-user", "uid": "start-user"})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := plugin.Providers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range ps {
		if p.ID != "zcode" {
			continue
		}
		if p.Spec != "builtin:zcode" {
			t.Fatalf("unexpected serving source: %+v", p)
		}
		for _, m := range p.Models {
			if strings.EqualFold(m.ID, "GLM-5.3-Flash") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("embedded model was not listed: %+v", ps)
	}
	captured, err := ZCodeAccountOperation(ctx, PluginID("zcode"), start, "context", nil)
	if err != nil || captured.Context.Kind != "start" || captured.Context.UserID != "start-user" || captured.Context.Scope == "" || captured.Context.Generation == "" {
		t.Fatalf("embedded Start context = %+v, %v", captured, err)
	}
	res, err := plugin.Fetch(ctx, plugin.FetchRequest{Provider: "zcode", Account: start, Model: "GLM-5.3-Flash", NPM: "@ai-sdk/anthropic",
		URL: "https://api.z.ai/api/anthropic/v1/messages", Method: http.MethodPost,
		Headers: map[string]string{"content-type": "application/json", "anthropic-version": "2023-06-01"},
		Body:    []byte(`{"model":"GLM-5.3-Flash","max_tokens":32,"system":"caller instruction","messages":[{"role":"user","content":"hello"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(res.Body)
	res.Body.Close()
	if readErr != nil || res.StatusCode != http.StatusOK || !strings.Contains(string(body), "start reply") {
		t.Fatalf("Start response: %d %s %v", res.StatusCode, body, readErr)
	}
	calls := vendor.Calls("/api/v1/zcode-plan/anthropic/v1/messages")
	if len(calls) != 1 || calls[0].Auth != "Bearer fixture-start-jwt" {
		t.Fatalf("Start wire calls = %+v", calls)
	}
	var wire struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
		Messages []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(calls[0].Body, &wire); err != nil || len(wire.System) != 3 || wire.System[0].Text != "You are ZCode, an interactive agent" ||
		!strings.Contains(wire.System[2].Text, "caller instruction") || len(wire.Messages) < 1 || len(wire.Messages[0].Content) < 3 ||
		!strings.Contains(wire.Messages[0].Content[0].Text, "# Environment") || !strings.Contains(wire.Messages[0].Content[1].Text, "GLM-5.3-Flash") {
		t.Fatalf("3.18 Start prompt/announcements not on host wire: %s (%v)", calls[0].Body, err)
	}
	usage, err := plugin.AccountUsage(ctx, "zcode", start)
	if err != nil {
		t.Fatal(err)
	}
	foundStart := false
	for _, o := range usage.Observations {
		if o.Kind == "start-balance" && o.Identity != nil && o.Identity.UserPlanID == "user-start" && o.Identity.PlanID == "start" && o.Identity.BucketID == "bucket-a" && o.Identity.EntitlementID == "ent-a" && o.Identity.PeriodStart != "" && o.Identity.PeriodEnd != "" && o.Remaining != nil && *o.Remaining == 75 {
			foundStart = true
		}
	}
	if !foundStart {
		t.Fatalf("Start bucket/plan/period lost: %+v", usage)
	}
	personal, err := plugin.Import(ctx, "zcode", fixture.Auth("fixture-business", "fixture-zcode"))
	if err != nil {
		t.Fatal(err)
	}
	mcp, err := ZCodeAccountOperation(ctx, PluginID("zcode"), personal, "usage.mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	var mcpObservation plugin.UsageObservation
	if err := json.Unmarshal(mcp.Data, &mcpObservation); err != nil || mcpObservation.Kind != "server-mcp" || mcpObservation.Used == nil || *mcpObservation.Used != 4 || mcpObservation.Remaining == nil || *mcpObservation.Remaining != 16 {
		t.Fatalf("Server MCP observation = %s, %v", mcp.Data, err)
	}
	personalContext, err := ZCodeAccountOperation(ctx, PluginID("zcode"), personal, "context", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = plugin.AccountOperation(ctx, "zcode", "zcode#missing", plugin.AccountRequest{Operation: "mcp.headers", Params: map[string]any{"url": "https://attacker.invalid/mcp"}})
	var accountErr *plugin.AccountError
	if !errors.As(err, &accountErr) || accountErr.Kind != "untrusted_url" {
		t.Fatalf("untrusted MCP did not fail before account lookup: %v", err)
	}
	_, err = plugin.AccountOperation(ctx, "zcode", "zcode#missing", plugin.AccountRequest{Operation: "mcp.headers", Params: map[string]any{"url": "https://zcode.z.ai/mcp"}})
	if !errors.As(err, &accountErr) || accountErr.Kind != "unavailable" {
		t.Fatalf("trusted MCP did not reach account lookup: %v", err)
	}
	trusted, err := plugin.AccountOperation(ctx, "zcode", personal, plugin.AccountRequest{Operation: "mcp.headers",
		Scope: personalContext.Context.Scope, Generation: personalContext.Context.Generation, Params: map[string]any{"url": "https://zcode.z.ai/mcp"}})
	var mcpAuth struct {
		Headers map[string]string `json:"headers"`
	}
	if err != nil || json.Unmarshal(trusted.Data, &mcpAuth) != nil || mcpAuth.Headers["Authorization"] != "Bearer fixture-zcode" {
		t.Fatalf("trusted MCP did not obtain the selected account's credential: %v", err)
	}
	ordinary := plugin.FetchRequest{Provider: "zcode", Account: personal, Model: "GLM-5.3", NPM: "@ai-sdk/anthropic",
		URL: "https://api.z.ai/api/anthropic/v1/messages", Method: http.MethodPost,
		Headers:    map[string]string{"content-type": "application/json", "anthropic-version": "2023-06-01"},
		Body:       []byte(`{"model":"GLM-5.3","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`),
		BundleSpec: "builtin:zcode", BundleTarget: plugin.Target("builtin:zcode"),
		ZCodeScope: personalContext.Context.Scope, ZCodeGeneration: personalContext.Context.Generation}
	res, err = plugin.Fetch(ctx, ordinary)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("same-generation ordinary request: HTTP %d", res.StatusCode)
	}
	before := len(vendor.Calls("/api/anthropic/v1/messages"))
	var imported struct {
		Account string `json:"account"`
	}
	if err := plugin.Call(ctx, "import", map[string]any{"provider": "zcode", "key": personal,
		"auth": fixture.Auth("fixture-business-new", "fixture-zcode-new")}, &imported); err != nil || imported.Account != personal {
		t.Fatalf("relogin = %+v, %v", imported, err)
	}
	res, err = plugin.Fetch(ctx, ordinary)
	if res != nil {
		res.Body.Close()
	}
	if err == nil || len(vendor.Calls("/api/anthropic/v1/messages")) != before {
		t.Fatalf("stale private fetch binding reached the ordinary vendor: before=%d after=%d err=%v", before, len(vendor.Calls("/api/anthropic/v1/messages")), err)
	}
	if len(vendor.Calls("/api/v1/mcp/usage")) == 0 || len(plugin.Load().Plugins) != 0 {
		t.Fatal("intrinsic proof used an external plugin or did not read MCP")
	}
	for _, call := range vendor.AllCalls() {
		if strings.Contains(strings.ToLower(call.Origin), "registry") {
			t.Fatal("intrinsic flow contacted a package registry")
		}
	}
}
