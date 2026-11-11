package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/plugin"
	zcodefixture "github.com/yetone/magpie/internal/plugin/testdata/nativeaccount"
	"github.com/yetone/magpie/internal/provider"
)

// These requests traverse the real embedded plugin and a loopback vendor;
// only the ordinary quota observation is supplied at the routing seam.
func TestZCodeFallbackGatewayPolicyAndLedger(t *testing.T) {
	fresh(t)
	vendor := zcodefixture.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, account := zcodefixture.Install(t, ctx)
	pid := provider.PluginID("zcode")
	captured, err := provider.ZCodeAccountOperation(ctx, pid, account, "context", nil)
	if err != nil {
		t.Fatal(err)
	}
	drawCtx := provider.WithZCodeAccountExpectation(ctx, captured.Context.Scope, captured.Context.Generation)
	if _, err := provider.ZCodeAccountOperation(drawCtx, pid, account, "execution.draw", map[string]any{"taskId": "task-1", "model": "GLM-5.3"}); err != nil {
		t.Fatalf("drawing fixture card: %v", err)
	}
	if _, _, err := provider.SetModelID(pid+"/GLM-5.3-Highspeed", "friendly"); err != nil {
		t.Fatal(err)
	}
	member := pid + "/friendly"
	if err := provider.SetModelEfforts(member, []string{"low", "high"}); err != nil {
		t.Fatal(err)
	}
	group := provider.Group{Name: "ZCode Fast", Members: []string{member}, Routing: provider.Ordered}
	group.SetMemberEffortMap(member, map[string]string{"high": "low"})
	if err := provider.SaveGroup(group); err != nil {
		t.Fatal(err)
	}
	oldAllowances := allowances
	t.Cleanup(func() { allowances = oldAllowances })
	allowances = func(string) map[string]provider.Allowance { return nil }
	s := New()
	ask := func(model, secret string, stream bool) *httptest.ResponseRecorder {
		t.Helper()
		body := `{"model":"` + model + `","max_tokens":64,"thinking":{"type":"enabled"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"ping"}]`
		if stream {
			body += `,"stream":true`
		}
		body += `}`
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		r.Header.Set("Anthropic-Version", "2023-06-01")
		r.Header.Set("X-Claude-Code-Session-Id", "task-1")
		if secret != "" {
			r.Header.Set("Authorization", "Bearer "+secret)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	const nativePath = "/api/v1/highspeed/anthropic/v1/messages"
	const ordinaryPath = "/api/anthropic/v1/messages"
	w := ask("group/zcode-fast", "", false)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ordinary reply") {
		t.Fatalf("allowed fallback: HTTP %d %s", w.Code, w.Body)
	}
	ordinary := vendor.Calls(ordinaryPath)
	if len(vendor.Calls(nativePath)) != 1 || len(ordinary) != 1 || ordinary[0].Model != "GLM-5.3" || ordinary[0].APIKey != "fixture-project-token" || ordinary[0].Marker != "" {
		t.Fatalf("native and ordinary upstream requests: native=%+v ordinary=%+v", vendor.Calls(nativePath), ordinary)
	}
	trace := lastRoute(s)
	if len(trace.Tries) != 1 || trace.Tries[0].Model != "GLM-5.3" || trace.Tries[0].ModelMappedFrom != "friendly" || trace.Tries[0].MappedFrom != "high" || trace.Tries[0].Effort != "low" {
		t.Fatalf("fallback trace lost actual model, alias, or effort mapping: %+v", trace.Tries)
	}
	ledger := lastUsage(t)
	if ledger.Model != "GLM-5.3" || ledger.Provider != pid || ledger.Input != 2 || ledger.Output != 3 {
		t.Fatalf("fallback ledger did not use the ordinary model: %+v", ledger)
	}

	logins := provider.Logins(pid)
	if len(logins) != 1 {
		t.Fatalf("fixture logins: %+v", logins)
	}
	user := logins[0].User
	// A 100% observation with no reset is still known exhausted. It does
	// not make the native attempt itself evidence of a spent native pool.
	allowances = func(string) map[string]provider.Allowance {
		return map[string]provider.Allowance{user: {{Model: "glm-5.3", Used: 100}}}
	}
	restingUntil.Lock()
	beforeRests := len(restingUntil.m)
	restingUntil.Unlock()
	w = ask(member, "", false)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(strings.ToLower(w.Body.String()), "ordinary model") {
		t.Fatalf("exhausted ordinary model: HTTP %d %s", w.Code, w.Body)
	}
	if len(vendor.Calls(nativePath)) != 2 || len(vendor.Calls(ordinaryPath)) != 1 {
		t.Fatalf("ordinary exhaustion sent paid traffic: native=%+v ordinary=%+v", vendor.Calls(nativePath), vendor.Calls(ordinaryPath))
	}
	restingUntil.Lock()
	afterRests := len(restingUntil.m)
	restingUntil.Unlock()
	if beforeRests != afterRests {
		t.Fatalf("local denial rested the native pool: %d -> %d", beforeRests, afterRests)
	}

	allowances = func(string) map[string]provider.Allowance { return nil }
	keys, secrets := newCaller(t, "restricted")
	if _, err := access.Update("models-key", access.Change{Key: keys[0].ID, Models: []string{pid + "/*"}}); err != nil {
		t.Fatal(err)
	}
	w = ask(member, secrets[0], false)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "implicitly change models") {
		t.Fatalf("restricted key fallback: HTTP %d %s", w.Code, w.Body)
	}
	if len(vendor.Calls(nativePath)) != 3 || len(vendor.Calls(ordinaryPath)) != 1 {
		t.Fatalf("restricted key sent ordinary traffic: native=%+v ordinary=%+v", vendor.Calls(nativePath), vendor.Calls(ordinaryPath))
	}

	vendor.SetPartial(true)
	w = ask(member, "", true)
	if !strings.Contains(w.Body.String(), "native partial") || len(vendor.Calls(ordinaryPath)) != 1 {
		t.Fatalf("accepted native stream was replayed: HTTP %d %s, ordinary=%+v", w.Code, w.Body, vendor.Calls(ordinaryPath))
	}
}

// A local denial of A's ordinary transition says nothing about B's
// separately admitted Highspeed lane.
func TestZCodeDeniedOrdinaryFallbackTriesNextNativeAccount(t *testing.T) {
	fresh(t)
	vendor := zcodefixture.Start(t)
	vendor.SetNativeSuccessAuth("fixture-zcode-b")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = zcodefixture.Install(t, ctx)
	_, err := plugin.Import(ctx, "zcode", zcodefixture.AuthFor("fixture-user-b", "fixture-business-b", "fixture-zcode-b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	pid := provider.PluginID("zcode")
	if err := provider.SetRouting(pid, provider.Ordered); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetAccountOrder(pid, []string{"fixture-user", "fixture-user-b"}); err != nil {
		t.Fatal(err)
	}
	ps, err := plugin.Providers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var accounts [2]string
	for _, p := range ps {
		if p.ID != "zcode" {
			continue
		}
		for _, a := range p.Accounts {
			switch a.AccountID {
			case "fixture-user":
				accounts[0] = a.Key
			case "fixture-user-b":
				accounts[1] = a.Key
			}
		}
	}
	if accounts[0] == "" || accounts[1] == "" || accounts[0] == accounts[1] {
		t.Fatalf("distinct account slots not found: %+v", accounts)
	}
	var scopes [2]string
	for i, account := range accounts {
		captured, err := provider.ZCodeAccountOperation(ctx, pid, account, "context", nil)
		if err != nil {
			t.Fatal(err)
		}
		drawCtx := provider.WithZCodeAccountExpectation(ctx, captured.Context.Scope, captured.Context.Generation)
		result, err := provider.ZCodeAccountOperation(drawCtx, pid, account, "execution.draw", map[string]any{"taskId": "task-1", "model": "GLM-5.3"})
		if err != nil {
			t.Fatalf("drawing account %d fixture card: %v", i, err)
		}
		scopes[i] = result.Context.Scope
	}
	if scopes[0] == "" || scopes[1] == "" || scopes[0] == scopes[1] {
		t.Fatalf("native admissions did not bind distinct scopes: %+v", scopes)
	}
	const nativePath = "/api/v1/highspeed/anthropic/v1/messages"
	const ordinaryPath = "/api/anthropic/v1/messages"
	oldAllowances := allowances
	t.Cleanup(func() { allowances = oldAllowances })
	allowances = func(string) map[string]provider.Allowance {
		// A's ordinary quota becomes exhausted after its native dispatch,
		// before the fallback guard evaluates the current observation.
		if len(vendor.Calls(nativePath)) == 0 {
			return nil
		}
		return map[string]provider.Allowance{"fixture-user": {{Model: "glm-5.3", Used: 100}}}
	}
	restingUntil.Lock()
	beforeRests := len(restingUntil.m)
	restingUntil.Unlock()
	s := New()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"`+pid+`/GLM-5.3-Highspeed","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-Claude-Code-Session-Id", "task-1")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "native reply from account B") {
		t.Fatalf("the next native account did not answer: HTTP %d %s", w.Code, w.Body)
	}
	calls := vendor.Calls(nativePath)
	if len(calls) != 2 || calls[0].Auth != "Bearer fixture-zcode" || calls[1].Auth != "Bearer fixture-zcode-b" {
		t.Fatalf("native account order: %+v", calls)
	}
	if paid := vendor.Calls(ordinaryPath); len(paid) != 0 {
		t.Fatalf("a denied ordinary fallback sent paid traffic: %+v", paid)
	}
	trace := lastRoute(s)
	if len(trace.Tries) != 2 || !strings.Contains(trace.Tries[0].Error, "ordinary model") ||
		trace.Tries[1].Model != "GLM-5.3-Highspeed" || trace.Tries[1].Status != http.StatusOK {
		t.Fatalf("local denial and native retry were not traced: %+v", trace.Tries)
	}
	restingUntil.Lock()
	afterRests := len(restingUntil.m)
	restingUntil.Unlock()
	if beforeRests != afterRests {
		t.Fatalf("local denial rested an account: %d -> %d", beforeRests, afterRests)
	}
}
